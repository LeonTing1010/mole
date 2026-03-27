use crate::{config, runner, store::Store, uri::ProxyNode};
use std::sync::atomic::Ordering;
use std::time::Instant;

struct BenchResult {
    name: String,
    ip: String,
    speed_kbps: f64,
    latency_ms: u64,
    ok: bool,
}

fn stop_bench_child(child: &mut std::process::Child) {
    let pid = child.id();
    let _ = std::process::Command::new("sudo")
        .args(["kill", &pid.to_string()])
        .output();
    runner::stop_singbox().ok();
    runner::SINGBOX_PID.store(0, Ordering::Relaxed);
}

pub fn run_bench() {
    let s = Store::load();
    if s.nodes.is_empty() {
        eprintln!("no nodes saved. use `mole add <uri>` first.");
        std::process::exit(1);
    }

    if !runner::singbox_installed() {
        eprintln!("sing-box not found, run `mole install` first.");
        std::process::exit(1);
    }

    // Kill any running instance
    runner::stop_singbox().ok();

    let total = s.nodes.len();
    println!();
    println!("  \x1b[1mbench\x1b[0m  testing {} nodes...", total);
    println!("  \x1b[2m─────────────────────────────────────────────────\x1b[0m");

    runner::mole_log("INFO", &format!("bench: testing {total} nodes"));

    let mut results: Vec<BenchResult> = Vec::new();

    for (i, node) in s.nodes.iter().enumerate() {
        let parsed = match ProxyNode::parse(&node.uri) {
            Ok(n) => n,
            Err(e) => {
                println!(
                    "  \x1b[31m✗\x1b[0m [{}/{}] {} — parse error: {e}",
                    i + 1,
                    total,
                    node.name
                );
                results.push(BenchResult {
                    name: node.name.clone(),
                    ip: String::new(),
                    speed_kbps: 0.0,
                    latency_ms: 9999,
                    ok: false,
                });
                continue;
            }
        };

        eprint!("  \x1b[33m…\x1b[0m [{}/{}] {} ", i + 1, total, node.name);

        let cfg = config::generate(&[("proxy", &parsed)], &s.rules, false, None, false);
        let json = config::to_json_pretty(&cfg);
        let config_path = match runner::write_config(&json) {
            Ok(p) => p,
            Err(_) => {
                println!("\x1b[31m— config error\x1b[0m");
                results.push(BenchResult {
                    name: node.name.clone(),
                    ip: String::new(),
                    speed_kbps: 0.0,
                    latency_ms: 9999,
                    ok: false,
                });
                continue;
            }
        };

        // Start sing-box
        let log_file = runner::log_path();
        let log = match std::fs::File::create(&log_file) {
            Ok(f) => f,
            Err(_) => {
                println!("\x1b[31m— log error\x1b[0m");
                continue;
            }
        };
        let log_err = log.try_clone().unwrap();

        let mut child = match std::process::Command::new("sudo")
            .arg(runner::singbox_bin_path().to_str().unwrap())
            .arg("run")
            .arg("-c")
            .arg(config_path.to_str().unwrap())
            .stdout(log)
            .stderr(log_err)
            .spawn()
        {
            Ok(c) => c,
            Err(_) => {
                println!("\x1b[31m— start error\x1b[0m");
                continue;
            }
        };

        // Track PID for panic-hook cleanup
        runner::SINGBOX_PID.store(child.id(), Ordering::Relaxed);

        // Wait for sing-box to start
        let mut started = false;
        for _ in 0..15 {
            std::thread::sleep(std::time::Duration::from_secs(1));
            if let Ok(log) = std::fs::read_to_string(&log_file) {
                if log.contains("sing-box started") {
                    started = true;
                    break;
                }
                if log.contains("FATAL") {
                    break;
                }
            }
        }

        if !started {
            eprint!("\r\x1b[K");
            println!(
                "  \x1b[31m✗\x1b[0m [{}/{}] {} — failed to start",
                i + 1,
                total,
                node.name
            );
            stop_bench_child(&mut child);
            results.push(BenchResult {
                name: node.name.clone(),
                ip: String::new(),
                speed_kbps: 0.0,
                latency_ms: 9999,
                ok: false,
            });
            continue;
        }

        // Test latency (time to first byte)
        let start = Instant::now();
        let ip = reqwest::blocking::Client::builder()
            .timeout(std::time::Duration::from_secs(10))
            .build()
            .ok()
            .and_then(|c| c.get("https://ipinfo.io/ip").send().ok())
            .and_then(|r| r.text().ok())
            .unwrap_or_default()
            .trim()
            .to_string();
        let latency_ms = start.elapsed().as_millis() as u64;

        // Test download speed (1MB)
        let start = Instant::now();
        let downloaded = reqwest::blocking::Client::builder()
            .timeout(std::time::Duration::from_secs(15))
            .build()
            .ok()
            .and_then(|c| c.get("https://proof.ovh.net/files/1Mb.dat").send().ok())
            .and_then(|r| r.bytes().ok())
            .map(|b| b.len())
            .unwrap_or(0);
        let elapsed = start.elapsed().as_secs_f64();
        let speed_kbps = if elapsed > 0.0 {
            (downloaded as f64 / 1024.0) / elapsed
        } else {
            0.0
        };

        // Stop sing-box
        stop_bench_child(&mut child);

        let ok = !ip.is_empty() && speed_kbps > 0.0;

        eprint!("\r\x1b[K");
        if ok {
            println!(
                "  \x1b[32m✓\x1b[0m [{}/{}] {:<20} {:>8.0} KB/s  {:>5}ms  {}",
                i + 1,
                total,
                node.name,
                speed_kbps,
                latency_ms,
                ip
            );
        } else {
            println!(
                "  \x1b[31m✗\x1b[0m [{}/{}] {} — timeout",
                i + 1,
                total,
                node.name
            );
        }

        results.push(BenchResult {
            name: node.name.clone(),
            ip,
            speed_kbps,
            latency_ms,
            ok,
        });
    }

    // Find fastest
    println!("  \x1b[2m─────────────────────────────────────────────────\x1b[0m");

    let best = results
        .iter()
        .filter(|r| r.ok)
        .max_by(|a, b| {
            a.speed_kbps
                .partial_cmp(&b.speed_kbps)
                .unwrap_or(std::cmp::Ordering::Equal)
        });

    match best {
        Some(winner) => {
            let mut s = Store::load();
            s.select(&winner.name);
            s.save().ok();
            runner::mole_log(
                "INFO",
                &format!(
                    "bench: winner={} speed={:.0}KB/s latency={}ms",
                    winner.name, winner.speed_kbps, winner.latency_ms
                ),
            );
            println!(
                "\n  \x1b[1;32m★\x1b[0m fastest: \x1b[1m{}\x1b[0m ({:.0} KB/s, {}ms, {})",
                winner.name, winner.speed_kbps, winner.latency_ms, winner.ip
            );
            println!("  \x1b[2mactivated. run `mole up` to connect.\x1b[0m\n");
        }
        None => {
            runner::mole_log("WARN", "bench: no working nodes found");
            println!("\n  \x1b[31mno working nodes found\x1b[0m\n");
        }
    }
}
