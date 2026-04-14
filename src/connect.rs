use std::path::PathBuf;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;

use crate::{config, platform, runner, status, store, uri};

// ── Helpers ─────────────────────────────────────────────────────────

/// Sanitize a node name into a valid sing-box outbound tag (ASCII only).
fn sanitize_tag(name: &str) -> String {
    let s: String = name
        .chars()
        .filter(|c| c.is_ascii_alphanumeric() || *c == '-' || *c == '_')
        .collect();
    if s.is_empty() {
        "node".to_string()
    } else {
        s
    }
}

/// Generate unique tags from node names, appending suffix on collision.
fn unique_tags(names: &[String]) -> Vec<String> {
    let mut seen = std::collections::HashMap::<String, usize>::new();
    names
        .iter()
        .map(|name| {
            let base = sanitize_tag(name);
            let count = seen.entry(base.clone()).or_insert(0);
            let tag = if *count == 0 {
                base.clone()
            } else {
                format!("{base}-{count}")
            };
            *count += 1;
            tag
        })
        .collect()
}

/// Find the next untried node for failover.
fn find_next_node(s: &store::Store, current: &str, tried: &[String]) -> Option<store::Node> {
    let mut name = current.to_string();
    for _ in 0..s.nodes.len() {
        match s.next_node(&name) {
            Some(next) if !tried.contains(&next.name) => return Some(next.clone()),
            Some(next) => name = next.name.clone(),
            None => return None,
        }
    }
    None
}

/// Print share info (proxy address + QR code) after connection is established.
fn print_share_info() {
    let ip = platform::local_ip().unwrap_or_else(|| "?.?.?.?".to_string());
    let port = config::SHARE_PORT;
    let proxy_url = format!("http://{ip}:{port}");

    eprintln!();
    eprintln!("  \x1b[1;32m  LAN proxy active\x1b[0m");
    eprintln!();
    eprintln!("  \x1b[2mhttp\x1b[0m    {ip}:{port}");
    eprintln!("  \x1b[2msocks5\x1b[0m  {ip}:{port}");
    eprintln!();

    // QR code for easy phone setup
    if crate::print_qr(&proxy_url) {
        eprintln!("  \x1b[2mscan QR or set Wi-Fi proxy to {ip}:{port}\x1b[0m");
    } else {
        eprintln!("  \x1b[2mset Wi-Fi HTTP proxy to {ip}:{port}\x1b[0m");
    }
    eprintln!();
}

fn print_banner() {
    println!();
    println!("  \x1b[1;36m  .--.\x1b[0m");
    println!(
        "  \x1b[1;36m / oo \\\x1b[0m   \x1b[1mmole\x1b[0m \x1b[2mv{}\x1b[0m",
        env!("CARGO_PKG_VERSION")
    );
    println!("  \x1b[1;36m( >  < )\x1b[0m   \x1b[2mTUN VPN powered by sing-box\x1b[0m");
    println!("  \x1b[1;36m  `--'\x1b[0m");
    println!();
}

/// Write config JSON, check geo files, validate via sing-box check.
fn prepare_config(
    nodes: &[(&str, &uri::ProxyNode)],
    rules: &[store::Rule],
    bypass_cn: bool,
    strategy: Option<&str>,
    share: bool,
) -> Result<PathBuf, String> {
    let cfg = config::generate(nodes, rules, bypass_cn, strategy, share);
    let json = config::to_json_pretty(&cfg);
    let path = runner::write_config(&json)?;
    runner::check_geo_files(rules, bypass_cn)?;
    runner::check_config(&path)?;
    Ok(path)
}

// ── Public entry points ─────────────────────────────────────────────

/// Run in strategy mode: all nodes as outbounds wrapped in a strategy.
/// Retries indefinitely since all nodes are in the config.
pub fn run_strategy(s: &store::Store, strat: &str, bypass_cn: bool, is_daemon: bool, share: bool) {
    let mut names: Vec<String> = Vec::new();
    let mut nodes_parsed: Vec<uri::ProxyNode> = Vec::new();
    for node in &s.nodes {
        match uri::ProxyNode::parse(&node.uri) {
            Ok(p) => {
                names.push(node.name.clone());
                nodes_parsed.push(p);
            }
            Err(e) => {
                if !is_daemon {
                    eprintln!("  skip {}: {e}", node.name);
                }
            }
        }
    }
    if nodes_parsed.is_empty() {
        eprintln!("no valid nodes");
        std::process::exit(1);
    }

    let tags = unique_tags(&names);
    let paired: Vec<(String, &uri::ProxyNode)> =
        tags.into_iter().zip(nodes_parsed.iter()).collect();
    let node_refs: Vec<(&str, &uri::ProxyNode)> =
        paired.iter().map(|(t, n)| (t.as_str(), *n)).collect();

    if !is_daemon {
        print_banner();
        let mode = if bypass_cn { "bypass-cn" } else { "global" };
        println!(
            "  \x1b[2mstrategy\x1b[0m \x1b[1;37m{strat}\x1b[0m ({} nodes)",
            nodes_parsed.len()
        );
        println!("  \x1b[2mmode\x1b[0m     {mode}");
        eprint!("  \x1b[2mstatus\x1b[0m   \x1b[33mconnecting...\x1b[0m");
    }

    let config_path = match prepare_config(&node_refs, &s.rules, bypass_cn, Some(strat), share) {
        Ok(p) => p,
        Err(e) => {
            eprintln!("\r\x1b[K  \x1b[31merror:\x1b[0m {e}");
            std::process::exit(1);
        }
    };

    runner::mole_log(
        "INFO",
        &format!(
            "started strategy={strat} nodes={} bypass_cn={bypass_cn}",
            nodes_parsed.len()
        ),
    );

    std::fs::remove_file(runner::log_path()).ok();
    let thread_stop = Arc::new(AtomicBool::new(false));
    let _keepalive = runner::start_keepalive(thread_stop.clone());
    if !is_daemon {
        status::start_live_monitor(thread_stop.clone());
    }
    if share && !is_daemon {
        print_share_info();
    }

    // All nodes are in config — sing-box handles internal failover.
    // If it crashes, keep restarting indefinitely.
    loop {
        match runner::run_singbox(&config_path) {
            Ok(runner::ExitReason::Clean) => {
                thread_stop.store(true, Ordering::SeqCst);
                runner::stop_singbox().ok();
                runner::mole_log("INFO", "stopped");
                break;
            }
            Ok(runner::ExitReason::MaxRetries) => {
                runner::mole_log(
                    "WARN",
                    "strategy mode: max retries reached, restarting cycle",
                );
                continue;
            }
            Err(e) => {
                thread_stop.store(true, Ordering::SeqCst);
                runner::stop_singbox().ok();
                eprintln!("error: {e}");
                std::process::exit(1);
            }
        }
    }
}

/// Run in single-node mode with failover to next node on MaxRetries.
pub fn run_single(s: &store::Store, bypass_cn: bool, is_daemon: bool, share: bool) {
    let node = match s.active_node() {
        Some(n) => n.clone(),
        None => {
            let first = s.nodes[0].clone();
            let mut s2 = store::Store::load();
            s2.select(&first.name);
            s2.save().ok();
            first
        }
    };

    let mut current_node = node;
    let mut tried_nodes: Vec<String> = vec![];
    let thread_stop = Arc::new(AtomicBool::new(false));
    let mut first_node = true;

    loop {
        let parsed = match uri::ProxyNode::parse(&current_node.uri) {
            Ok(n) => n,
            Err(e) => {
                eprintln!("error: {e}");
                std::process::exit(1);
            }
        };

        let mode = if bypass_cn { "bypass-cn" } else { "global" };
        let protocol = parsed.protocol();

        if !is_daemon {
            if first_node {
                print_banner();
                println!(
                    "  \x1b[2mnode\x1b[0m    \x1b[1;37m{}\x1b[0m",
                    current_node.name
                );
                println!(
                    "  \x1b[2mserver\x1b[0m  {protocol}://{}",
                    parsed.server_addr()
                );
                println!("  \x1b[2mmode\x1b[0m    {mode}");
                eprint!("  \x1b[2mstatus\x1b[0m  \x1b[33mconnecting...\x1b[0m");
                first_node = false;
            } else {
                eprintln!(
                    "\n  \x1b[33mfailover\x1b[0m → \x1b[1;37m{}\x1b[0m ({protocol}://{})",
                    current_node.name,
                    parsed.server_addr()
                );
                eprint!("  \x1b[2mstatus\x1b[0m  \x1b[33mconnecting...\x1b[0m");
            }
        }

        let config_path =
            match prepare_config(&[("proxy", &parsed)], &s.rules, bypass_cn, None, share) {
                Ok(p) => p,
                Err(e) => {
                    runner::mole_log(
                        "ERROR",
                        &format!("config failed for {}: {e}", current_node.name),
                    );
                    eprintln!("\r\x1b[K  \x1b[31merror:\x1b[0m {e}");
                    tried_nodes.push(current_node.name.clone());
                    let s = store::Store::load();
                    match find_next_node(&s, &current_node.name, &tried_nodes) {
                        Some(next) => {
                            current_node = next;
                            continue;
                        }
                        None => {
                            eprintln!("  \x1b[31mno more nodes to try\x1b[0m");
                            std::process::exit(1);
                        }
                    }
                }
            };

        runner::mole_log(
            "INFO",
            &format!(
                "started node={} proto={protocol} mode={mode}",
                current_node.name
            ),
        );

        std::fs::remove_file(runner::log_path()).ok();
        let _keepalive = runner::start_keepalive(thread_stop.clone());
        if !is_daemon {
            status::start_live_monitor(thread_stop.clone());
        }
        if share && !is_daemon && tried_nodes.is_empty() {
            print_share_info();
        }

        match runner::run_singbox(&config_path) {
            Ok(runner::ExitReason::Clean) => {
                thread_stop.store(true, Ordering::SeqCst);
                runner::stop_singbox().ok();
                runner::mole_log("INFO", "stopped (user initiated)");
                break;
            }
            Ok(runner::ExitReason::MaxRetries) => {
                // Signal old keepalive/monitor threads to stop, then reset for next node
                thread_stop.store(true, Ordering::SeqCst);
                runner::stop_singbox().ok();
                // Wait for old threads to see stop signal (keepalive checks every 30s,
                // but is sleeping — worst case we wait a bit longer than needed)
                std::thread::sleep(std::time::Duration::from_secs(2));
                thread_stop.store(false, Ordering::SeqCst);

                tried_nodes.push(current_node.name.clone());
                let s = store::Store::load();
                match find_next_node(&s, &current_node.name, &tried_nodes) {
                    Some(next) => {
                        runner::mole_log(
                            "INFO",
                            &format!("failover: {} -> {}", current_node.name, next.name),
                        );
                        current_node = next;
                        continue;
                    }
                    None => {
                        runner::mole_log("ERROR", "all nodes exhausted");
                        eprintln!("\n  \x1b[31mall nodes exhausted, giving up\x1b[0m");
                        std::process::exit(1);
                    }
                }
            }
            Err(e) => {
                thread_stop.store(true, Ordering::SeqCst);
                runner::stop_singbox().ok();
                runner::mole_log("ERROR", &format!("fatal: {e}"));
                eprintln!("error: {e}");
                std::process::exit(1);
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn sanitize_tag_ascii() {
        assert_eq!(sanitize_tag("tokyo-node"), "tokyo-node");
        assert_eq!(sanitize_tag("node_1"), "node_1");
    }

    #[test]
    fn sanitize_tag_cjk_produces_nonempty() {
        // CJK characters are filtered out, fallback to "node"
        assert_eq!(sanitize_tag("东京节点"), "node");
    }

    #[test]
    fn sanitize_tag_mixed() {
        assert_eq!(sanitize_tag("US-节点-1"), "US--1");
    }

    #[test]
    fn unique_tags_no_collision() {
        let names = vec!["tokyo".into(), "london".into()];
        let tags = unique_tags(&names);
        assert_eq!(tags, vec!["tokyo", "london"]);
    }

    #[test]
    fn unique_tags_cjk_collision() {
        // Multiple CJK-only names all sanitize to "node" — must be deduped
        let names = vec!["东京节点".into(), "新加坡节点".into(), "伦敦节点".into()];
        let tags = unique_tags(&names);
        assert_eq!(tags[0], "node");
        assert_eq!(tags[1], "node-1");
        assert_eq!(tags[2], "node-2");
        // All unique
        let mut sorted = tags.clone();
        sorted.sort();
        sorted.dedup();
        assert_eq!(sorted.len(), 3);
    }

    #[test]
    fn unique_tags_partial_collision() {
        let names = vec!["US-1".into(), "US-1".into(), "JP-1".into()];
        let tags = unique_tags(&names);
        assert_eq!(tags[0], "US-1");
        assert_eq!(tags[1], "US-1-1");
        assert_eq!(tags[2], "JP-1");
    }
}
