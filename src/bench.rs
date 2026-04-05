use crate::{config, runner, store::Store, uri::ProxyNode};
use std::sync::atomic::Ordering;
use std::time::Instant;

pub struct TestResult {
    pub ip: String,
    pub latency_ms: u64,
    pub ok: bool,
}

fn stop_child(child: &mut std::process::Child) {
    let pid = child.id();
    let _ = std::process::Command::new("sudo")
        .args(["kill", &pid.to_string()])
        .output();
    runner::stop_singbox().ok();
    runner::SINGBOX_PID.store(0, Ordering::Relaxed);
}

/// Test a single node's connectivity. Returns None on config/start error.
pub fn test_node(node: &ProxyNode, rules: &[crate::store::Rule]) -> TestResult {
    let cfg = config::generate(&[("proxy", node)], rules, false, None, false);
    let json = config::to_json_pretty(&cfg);
    let config_path = match runner::write_config(&json) {
        Ok(p) => p,
        Err(_) => {
            return TestResult {
                ip: String::new(),
                latency_ms: 9999,
                ok: false,
            }
        }
    };

    let log_file = runner::log_path();
    let log = match std::fs::File::create(&log_file) {
        Ok(f) => f,
        Err(_) => {
            return TestResult {
                ip: String::new(),
                latency_ms: 9999,
                ok: false,
            }
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
            return TestResult {
                ip: String::new(),
                latency_ms: 9999,
                ok: false,
            }
        }
    };

    runner::SINGBOX_PID.store(child.id(), Ordering::Relaxed);

    // Wait for sing-box to start
    let mut started = false;
    for _ in 0..15 {
        std::thread::sleep(std::time::Duration::from_secs(1));
        if let Ok(Some(_)) = child.try_wait() {
            break;
        }
        if std::net::TcpStream::connect_timeout(
            &"127.0.0.1:19090".parse().unwrap(),
            std::time::Duration::from_millis(500),
        )
        .is_ok()
        {
            started = true;
            break;
        }
    }

    if !started {
        stop_child(&mut child);
        return TestResult {
            ip: String::new(),
            latency_ms: 9999,
            ok: false,
        };
    }

    let start = Instant::now();
    let ip = reqwest::blocking::Client::builder()
        .timeout(std::time::Duration::from_secs(5))
        .build()
        .ok()
        .and_then(|c| c.get("https://ipinfo.io/ip").send().ok())
        .and_then(|r| r.text().ok())
        .unwrap_or_default()
        .trim()
        .to_string();
    let latency_ms = start.elapsed().as_millis() as u64;

    stop_child(&mut child);

    let ok = !ip.is_empty();
    TestResult { ip, latency_ms, ok }
}

/// Test a single node without sudo — uses mixed inbound (SOCKS5 proxy) instead of TUN.
pub fn test_node_nosudo(node: &ProxyNode) -> TestResult {
    test_node_on_port(node, config::TEST_PORT)
}

/// Test a node on a specific port (for concurrent bench).
fn test_node_on_port(node: &ProxyNode, port: u16) -> TestResult {
    let cfg = config::generate_test_on_port(node, port);
    let json = config::to_json_pretty(&cfg);

    let mole_dir = runner::mole_dir();
    let config_file = mole_dir.join(format!("bench-{port}.json"));
    let log_file = mole_dir.join(format!("bench-{port}.log"));

    if std::fs::write(&config_file, &json).is_err() {
        return fail_result();
    }

    let log = match std::fs::File::create(&log_file) {
        Ok(f) => f,
        Err(_) => return fail_result(),
    };
    let log_err = log.try_clone().unwrap();

    let mut child = match std::process::Command::new(runner::singbox_bin_path())
        .arg("run")
        .arg("-c")
        .arg(config_file.to_str().unwrap())
        .stdout(log)
        .stderr(log_err)
        .spawn()
    {
        Ok(c) => c,
        Err(_) => return fail_result(),
    };

    let addr = format!("127.0.0.1:{port}");
    let mut started = false;
    for _ in 0..10 {
        std::thread::sleep(std::time::Duration::from_secs(1));
        if let Ok(Some(_)) = child.try_wait() {
            break;
        }
        if std::net::TcpStream::connect_timeout(
            &addr.parse().unwrap(),
            std::time::Duration::from_millis(500),
        )
        .is_ok()
        {
            started = true;
            break;
        }
    }

    if !started {
        child.kill().ok();
        child.wait().ok();
        cleanup_bench_files(port);
        return fail_result();
    }

    let proxy_url = format!("socks5://127.0.0.1:{port}");
    let start = Instant::now();
    let ip = reqwest::blocking::Client::builder()
        .proxy(reqwest::Proxy::all(&proxy_url).unwrap())
        .timeout(std::time::Duration::from_secs(10))
        .build()
        .ok()
        .and_then(|c| c.get("https://ipinfo.io/ip").send().ok())
        .and_then(|r| r.text().ok())
        .unwrap_or_default()
        .trim()
        .to_string();
    let latency_ms = start.elapsed().as_millis() as u64;

    child.kill().ok();
    child.wait().ok();
    cleanup_bench_files(port);

    let ok = !ip.is_empty();
    TestResult { ip, latency_ms, ok }
}

fn fail_result() -> TestResult {
    TestResult {
        ip: String::new(),
        latency_ms: 9999,
        ok: false,
    }
}

fn cleanup_bench_files(port: u16) {
    let dir = runner::mole_dir();
    std::fs::remove_file(dir.join(format!("bench-{port}.json"))).ok();
    std::fs::remove_file(dir.join(format!("bench-{port}.log"))).ok();
}

/// Concurrency for bench: test up to PARALLEL nodes at once.
const PARALLEL: usize = 4;
const BENCH_PORT_BASE: u16 = 18200;

pub fn run_bench(clean: bool) {
    let s = Store::load();
    if s.nodes.is_empty() {
        eprintln!("no nodes saved. use `mole add <uri>` first.");
        std::process::exit(1);
    }

    if !runner::singbox_installed() {
        eprintln!("sing-box not found, run `mole install` first.");
        std::process::exit(1);
    }

    // Ctrl+C: clean up sing-box before exit
    ctrlc::set_handler(move || {
        if runner::SHUTTING_DOWN.swap(true, Ordering::SeqCst) {
            std::process::exit(130);
        }
        runner::stop_singbox().ok();
        std::process::exit(130);
    })
    .ok();

    // Kill any running instance
    runner::stop_singbox().ok();

    let total = s.nodes.len();
    println!();
    println!(
        "  \x1b[1mbench\x1b[0m  testing {} nodes ({}x parallel, no sudo)...",
        total, PARALLEL
    );
    println!("  \x1b[2m─────────────────────────────────────────────────\x1b[0m");

    runner::mole_log("INFO", &format!("bench: testing {total} nodes"));

    // Pre-parse all nodes
    let parsed: Vec<(String, Option<ProxyNode>)> = s
        .nodes
        .iter()
        .map(|n| (n.name.clone(), ProxyNode::parse(&n.uri).ok()))
        .collect();

    let mut results: Vec<(String, TestResult)> = Vec::with_capacity(total);

    // Process in chunks of PARALLEL
    for chunk_start in (0..total).step_by(PARALLEL) {
        let chunk_end = std::cmp::min(chunk_start + PARALLEL, total);
        let chunk = &parsed[chunk_start..chunk_end];

        // Spawn threads for this chunk
        let handles: Vec<_> = chunk
            .iter()
            .enumerate()
            .map(|(slot, (name, maybe_node))| {
                let port = BENCH_PORT_BASE + slot as u16;
                let name = name.clone();
                let node = maybe_node.clone();
                let idx = chunk_start + slot;

                std::thread::spawn(move || {
                    let result = match node {
                        Some(n) => test_node_on_port(&n, port),
                        None => fail_result(),
                    };
                    (idx, name, result)
                })
            })
            .collect();

        // Collect results and print
        for handle in handles {
            if let Ok((idx, name, r)) = handle.join() {
                if r.ok {
                    println!(
                        "  \x1b[32m✓\x1b[0m [{}/{}] {:<20} {:>5}ms  {}",
                        idx + 1,
                        total,
                        name,
                        r.latency_ms,
                        r.ip
                    );
                } else {
                    println!(
                        "  \x1b[31m✗\x1b[0m [{}/{}] {} — failed",
                        idx + 1,
                        total,
                        name
                    );
                }
                results.push((name, r));
            }
        }
    }

    // Find fastest
    println!("  \x1b[2m─────────────────────────────────────────────────\x1b[0m");

    let best = results
        .iter()
        .filter(|r| r.1.ok)
        .min_by_key(|r| r.1.latency_ms);

    match best {
        Some((name, r)) => {
            let mut s = Store::load();
            s.select(name);
            s.save().ok();
            runner::mole_log(
                "INFO",
                &format!("bench: winner={} latency={}ms", name, r.latency_ms),
            );
            println!(
                "\n  \x1b[1;32m★\x1b[0m fastest: \x1b[1m{}\x1b[0m ({}ms, {})",
                name, r.latency_ms, r.ip
            );
            println!("  \x1b[2mactivated. run `mole up` to connect.\x1b[0m\n");
        }
        None => {
            runner::mole_log("WARN", "bench: no working nodes found");
            println!("\n  \x1b[31mno working nodes found\x1b[0m\n");
        }
    }

    // Clean up failed nodes
    if clean {
        let failed: Vec<&str> = results
            .iter()
            .filter(|r| !r.1.ok)
            .map(|r| r.0.as_str())
            .collect();
        if !failed.is_empty() {
            let mut s = Store::load();
            for name in &failed {
                s.remove(name);
            }
            s.save().ok();
            println!("  \x1b[2mcleaned {} failed nodes\x1b[0m\n", failed.len());
        }
    }
}
