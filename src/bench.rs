use crate::{config, runner, store, uri::ProxyNode};
use std::io::Read as _;
use std::sync::atomic::Ordering;
use std::time::Instant;

pub struct TestResult {
    pub ip: String,
    pub latency_ms: u64,
    pub speed_kbps: u64,
    pub ipv6: bool,
    pub ok: bool,
}

/// Test a single node without sudo — uses mixed inbound (SOCKS5 proxy) instead of TUN.
/// Only checks connectivity + latency (no download speed test).
pub fn test_node_nosudo(node: &ProxyNode) -> TestResult {
    test_node_on_port(node, config::TEST_PORT, false)
}

/// Test a node on a specific port (for concurrent bench).
fn test_node_on_port(node: &ProxyNode, port: u16, measure_speed: bool) -> TestResult {
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
    for _ in 0..25 {
        std::thread::sleep(std::time::Duration::from_millis(200));
        if let Ok(Some(_)) = child.try_wait() {
            break;
        }
        if std::net::TcpStream::connect_timeout(
            &addr.parse().unwrap(),
            std::time::Duration::from_millis(200),
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
    let client = match reqwest::blocking::Client::builder()
        .proxy(reqwest::Proxy::all(&proxy_url).unwrap())
        .timeout(std::time::Duration::from_secs(5))
        .build()
    {
        Ok(c) => c,
        Err(_) => {
            child.kill().ok();
            child.wait().ok();
            cleanup_bench_files(port);
            return fail_result();
        }
    };

    // Get IP
    let start = Instant::now();
    let ip = client
        .get("https://ipinfo.io/ip")
        .send()
        .ok()
        .and_then(|r| r.text().ok())
        .unwrap_or_default()
        .trim()
        .to_string();
    let latency_ms = start.elapsed().as_millis() as u64;

    if ip.is_empty() {
        child.kill().ok();
        child.wait().ok();
        cleanup_bench_files(port);
        return fail_result();
    }

    // Download speed test: stream for up to 5 seconds
    let speed_kbps = if measure_speed {
        measure_download_speed(&proxy_url)
    } else {
        0
    };

    // IPv6 support check (always test — single fast request)
    let ipv6 = client
        .get("https://api6.ipify.org")
        .send()
        .ok()
        .and_then(|r| r.text().ok())
        .is_some_and(|t| t.trim().contains(':'));

    child.kill().ok();
    child.wait().ok();
    cleanup_bench_files(port);

    TestResult {
        ip,
        latency_ms,
        speed_kbps,
        ipv6,
        ok: true,
    }
}

/// Download a test file through the proxy for up to 5 seconds, return speed in KB/s.
/// Tries multiple CDN URLs in case some are blocked by the proxy.
fn measure_download_speed(proxy_url: &str) -> u64 {
    let client = match reqwest::blocking::Client::builder()
        .proxy(reqwest::Proxy::all(proxy_url).unwrap())
        .connect_timeout(std::time::Duration::from_secs(5))
        .build()
    {
        Ok(c) => c,
        Err(_) => return 0,
    };

    let urls = [
        "https://speed.cloudflare.com/__down?bytes=10000000",
        "http://cachefly.cachefly.net/10mb.test",
        "https://dl.google.com/linux/direct/google-chrome-stable_current_amd64.deb",
    ];

    for url in &urls {
        let mut resp = match client.get(*url).send() {
            Ok(r) if r.status().is_success() => r,
            _ => continue,
        };

        let start = Instant::now();
        let mut total_bytes: u64 = 0;
        let mut buf = [0u8; 16384];
        loop {
            if start.elapsed().as_secs() >= 5 {
                break;
            }
            match resp.read(&mut buf) {
                Ok(0) => break,
                Ok(n) => total_bytes += n as u64,
                Err(_) => break,
            }
        }
        if total_bytes > 0 {
            let elapsed_ms = start.elapsed().as_millis().max(1) as u64;
            return total_bytes * 1000 / elapsed_ms / 1024; // KB/s
        }
    }
    0
}

fn fail_result() -> TestResult {
    TestResult {
        ip: String::new(),
        latency_ms: 9999,
        speed_kbps: 0,
        ipv6: false,
        ok: false,
    }
}

fn cleanup_bench_files(port: u16) {
    let dir = runner::mole_dir();
    std::fs::remove_file(dir.join(format!("bench-{port}.json"))).ok();
    std::fs::remove_file(dir.join(format!("bench-{port}.log"))).ok();
}

/// Concurrency for bench/filter: test up to PARALLEL nodes at once.
const PARALLEL: usize = 8;
const BENCH_PORT_BASE: u16 = 18200;

pub fn parallel_count() -> usize {
    PARALLEL
}

/// Filter working nodes in parallel: test connectivity + IPv6 detection.
/// Returns all working nodes (v4 and v6). Prints progress inline.
pub fn filter_parallel(nodes: &[(String, String)]) -> Vec<(String, TestResult)> {
    let total = nodes.len();
    let mut results: Vec<(String, TestResult)> = Vec::new();

    let parsed: Vec<(String, String, Option<ProxyNode>)> = nodes
        .iter()
        .map(|(name, uri)| (name.clone(), uri.clone(), ProxyNode::parse(uri).ok()))
        .collect();

    for chunk_start in (0..total).step_by(PARALLEL) {
        let chunk_end = std::cmp::min(chunk_start + PARALLEL, total);
        let chunk = &parsed[chunk_start..chunk_end];

        let handles: Vec<_> = chunk
            .iter()
            .enumerate()
            .map(|(slot, (name, _uri, maybe_node))| {
                let port = BENCH_PORT_BASE + slot as u16;
                let name = name.clone();
                let node = maybe_node.clone();
                let idx = chunk_start + slot;

                std::thread::spawn(move || {
                    let result = match node {
                        Some(n) => test_node_on_port(&n, port, false),
                        None => fail_result(),
                    };
                    (idx, name, result)
                })
            })
            .collect();

        for handle in handles {
            if let Ok((idx, name, r)) = handle.join() {
                if r.ok {
                    let v6 = if r.ipv6 { "\x1b[36m6\x1b[0m" } else { "4" };
                    eprint!(
                        "\r  \x1b[32m✓\x1b[0m [{}/{}] v{} {}          \n",
                        idx + 1, total, v6, name
                    );
                    results.push((name, r));
                } else {
                    eprint!(
                        "\r  \x1b[31m✗\x1b[0m [{}/{}] {}          \n",
                        idx + 1, total, name
                    );
                }
            }
        }
    }

    results
}

/// Bench download speed for nodes in parallel. Returns (name, speed_kbps) sorted by speed desc.
pub fn bench_speed_parallel(nodes: &[(String, String)]) -> Vec<(String, u64)> {
    let total = nodes.len();
    let mut results: Vec<(String, u64)> = Vec::new();

    let parsed: Vec<(String, Option<ProxyNode>)> = nodes
        .iter()
        .map(|(name, uri)| (name.clone(), ProxyNode::parse(uri).ok()))
        .collect();

    for chunk_start in (0..total).step_by(PARALLEL) {
        let chunk_end = std::cmp::min(chunk_start + PARALLEL, total);
        let chunk = &parsed[chunk_start..chunk_end];

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
                        Some(n) => test_node_on_port(&n, port, true),
                        None => fail_result(),
                    };
                    (idx, name, result)
                })
            })
            .collect();

        for handle in handles {
            if let Ok((idx, name, r)) = handle.join() {
                if r.ok && r.speed_kbps > 0 {
                    let v6 = if r.ipv6 { "\x1b[36m6\x1b[0m" } else { "4" };
                    println!(
                        "  \x1b[32m✓\x1b[0m [{}/{}] v{} {:<28} {:>5} KB/s",
                        idx + 1, total, v6, name, r.speed_kbps
                    );
                    // Save bench result immediately
                    let mut bench = store::load_bench();
                    bench.insert(name.clone(), store::BenchEntry {
                        speed_kbps: r.speed_kbps,
                        ipv6: r.ipv6,
                    });
                    store::save_bench(&bench);
                    results.push((name, r.speed_kbps));
                } else if r.ok {
                    println!(
                        "  \x1b[2m·\x1b[0m [{}/{}]    {:<28}     0 KB/s",
                        idx + 1, total, name
                    );
                } else {
                    println!(
                        "  \x1b[31m✗\x1b[0m [{}/{}] {} — failed",
                        idx + 1, total, name
                    );
                }
            }
        }
    }

    results.sort_by(|a, b| b.1.cmp(&a.1));
    results
}

/// Discover mode: test nodes with speed, call `on_pass` for each passing node immediately.
/// This allows saving each node as soon as it passes — survives Ctrl+C.
pub fn bench_discover(nodes: &[(String, String)], mut on_pass: impl FnMut(&str, u64, bool)) {
    let total = nodes.len();
    let parsed: Vec<(String, Option<ProxyNode>)> = nodes
        .iter()
        .map(|(name, uri)| (name.clone(), ProxyNode::parse(uri).ok()))
        .collect();

    for chunk_start in (0..total).step_by(PARALLEL) {
        let chunk_end = std::cmp::min(chunk_start + PARALLEL, total);
        let chunk = &parsed[chunk_start..chunk_end];

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
                        Some(n) => test_node_on_port(&n, port, true),
                        None => fail_result(),
                    };
                    (idx, name, result)
                })
            })
            .collect();

        for handle in handles {
            if let Ok((idx, name, r)) = handle.join() {
                if r.ok && r.speed_kbps > 0 {
                    let v6 = if r.ipv6 { "\x1b[36m6\x1b[0m" } else { "4" };
                    println!(
                        "  \x1b[32m✓\x1b[0m [{}/{}] v{} {:<28} {:>5} KB/s",
                        idx + 1, total, v6, name, r.speed_kbps
                    );
                    // Save bench result
                    let mut bench = store::load_bench();
                    bench.insert(name.clone(), store::BenchEntry {
                        speed_kbps: r.speed_kbps,
                        ipv6: r.ipv6,
                    });
                    store::save_bench(&bench);
                    // Callback to save node
                    on_pass(&name, r.speed_kbps, r.ipv6);
                } else if r.ok {
                    println!(
                        "  \x1b[2m·\x1b[0m [{}/{}]    {:<28}     0 KB/s",
                        idx + 1, total, name
                    );
                } else {
                    println!(
                        "  \x1b[31m✗\x1b[0m [{}/{}] {} — failed",
                        idx + 1, total, name
                    );
                }
            }
        }
    }
}

pub fn run_bench(clean: bool) {
    let s = store::Store::load();
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
                        Some(n) => test_node_on_port(&n, port, true),
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
                    let v6 = if r.ipv6 { "\x1b[36m6\x1b[0m" } else { "4" };
                    println!(
                        "  \x1b[32m✓\x1b[0m [{}/{}] v{} {:<20} {:>5} KB/s  {}",
                        idx + 1,
                        total,
                        v6,
                        name,
                        r.speed_kbps,
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

    // Save bench results to separate file
    {
        let mut bench = store::load_bench();
        for (name, r) in &results {
            if r.ok {
                bench.insert(
                    name.clone(),
                    store::BenchEntry {
                        speed_kbps: r.speed_kbps,
                        ipv6: r.ipv6,
                    },
                );
            } else {
                bench.remove(name);
            }
        }
        store::save_bench(&bench);
    }

    let best = results
        .iter()
        .filter(|r| r.1.ok)
        .max_by_key(|r| r.1.speed_kbps);

    match best {
        Some((name, r)) => {
            let mut s = store::Store::load();
            s.select(name);
            s.save().ok();
            runner::mole_log(
                "INFO",
                &format!("bench: winner={} speed={}KB/s", name, r.speed_kbps),
            );
            println!(
                "\n  \x1b[1;32m★\x1b[0m fastest: \x1b[1m{}\x1b[0m ({} KB/s, {})",
                name, r.speed_kbps, r.ip
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
            let mut s = store::Store::load();
            for name in &failed {
                s.remove(name);
            }
            s.save().ok();
            println!("  \x1b[2mcleaned {} failed nodes\x1b[0m\n", failed.len());
        }
    }
}
