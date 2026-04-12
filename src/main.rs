mod bench;
mod config;
mod connect;
mod platform;
mod runner;
mod status;
mod store;
mod sub;
mod uri;

use std::sync::atomic::Ordering;

use clap::{Parser, Subcommand};
use store::Store;

#[derive(Parser)]
#[command(
    name = "mole",
    version,
    about = "Multi-protocol TUN VPN client (powered by sing-box)"
)]
struct Cli {
    #[command(subcommand)]
    command: Commands,
}

#[derive(Subcommand)]
enum Commands {
    /// Add a node (hysteria2://, hysteria://, vmess://, vless://, trojan://, ss://, tuic://, wg://)
    Add {
        uri: String,
        #[arg(short, long)]
        name: Option<String>,
        /// Test connectivity before adding
        #[arg(short, long)]
        test: bool,
    },
    /// Connect to the active node
    Up {
        #[arg(long, default_value_t = true, action = clap::ArgAction::Set)]
        bypass_cn: bool,
        #[arg(short, long)]
        daemon: bool,
        #[arg(long)]
        strategy: Option<String>,
        /// Share proxy to LAN devices (HTTP/SOCKS on port 7890)
        #[arg(long)]
        share: bool,
    },
    /// Disconnect (stop sing-box)
    Down,
    /// List saved nodes
    #[command(name = "ls")]
    List {
        /// Group nodes by subscription source
        #[arg(long)]
        by_source: bool,
        /// Hierarchical view: source > country > nodes
        #[arg(long, short)]
        tree: bool,
        /// Tree depth level (1=source only, 2=source+country, 3=full)
        #[arg(long, short = 'L')]
        level: Option<usize>,
    },
    /// Switch active node
    Use { name: String },
    /// Remove a saved node
    #[command(name = "rm")]
    Remove { name: String },
    /// Normalize node names to use unified format
    Rename,
    /// Generate QR code for a node (scan with phone to add)
    Qr {
        /// Node name (defaults to active node)
        name: Option<String>,
    },
    /// Test node connectivity (no sudo required)
    Test {
        /// Test all nodes instead of just the active one
        #[arg(long)]
        all: bool,
    },
    /// Benchmark all nodes and activate the fastest
    Bench {
        /// Remove failed nodes after benchmark
        #[arg(long)]
        clean: bool,
    },
    /// Show connection status, IP, and latency
    Status,
    /// Download the sing-box binary
    Install,
    /// Print generated sing-box config for the active node (dry run)
    Config,
    /// Manage subscriptions
    Sub {
        #[command(subcommand)]
        action: SubCommands,
    },
    /// Manage routing rules
    Rule {
        #[command(subcommand)]
        action: RuleCommands,
    },
    /// Manage system service (launchd/systemd)
    Service {
        #[command(subcommand)]
        action: ServiceCommands,
    },
}

#[derive(Subcommand)]
enum SubCommands {
    /// Add a subscription URL
    Add {
        url: String,
        #[arg(short, long)]
        name: Option<String>,
    },
    /// Update all subscriptions
    Update {
        /// Test and keep only working nodes
        #[arg(short, long)]
        test: bool,
    },
    /// List subscriptions
    Ls,
    /// Remove a subscription and its nodes
    Rm { name: String },
    /// Auto-discover from configured sources, keep only IPv6 nodes
    Discover,
    /// Manage discover sources
    Source {
        #[command(subcommand)]
        action: SourceCommands,
    },
}

#[derive(Subcommand)]
enum SourceCommands {
    /// Add a discover source
    Add {
        url: String,
        #[arg(short, long)]
        name: Option<String>,
        /// Source is an HTML page (extract proxy URIs from content)
        #[arg(long)]
        html: bool,
    },
    /// List discover sources
    Ls,
    /// Remove a discover source
    Rm { name: String },
}

#[derive(Subcommand)]
enum RuleCommands {
    /// Add a routing rule (types: domain, domain_suffix, domain_keyword, ip_cidr, geoip, geosite)
    Add {
        match_type: String,
        pattern: String,
        outbound: String,
    },
    /// List all rules
    Ls,
    /// Remove a rule by index
    Rm { index: usize },
    /// Remove all rules
    Clear,
}

#[derive(Subcommand)]
enum ServiceCommands {
    /// Install as system service (launchd on macOS, systemd on Linux)
    Install,
    /// Uninstall system service
    Uninstall,
}

fn main() {
    let default_hook = std::panic::take_hook();
    std::panic::set_hook(Box::new(move |info| {
        runner::stop_singbox().ok();
        default_hook(info);
    }));

    let cli = Cli::parse();

    match cli.command {
        Commands::Install => {
            if let Err(e) = runner::install_singbox() {
                eprintln!("error: {e}");
                std::process::exit(1);
            }
        }

        Commands::Add {
            uri: raw,
            name,
            test,
        } => {
            let node = match uri::ProxyNode::parse(&raw) {
                Ok(n) => n,
                Err(e) => {
                    eprintln!("error: {e}");
                    std::process::exit(1);
                }
            };
            let node_name = name
                .or_else(|| node.name().map(|s| s.to_string()))
                .unwrap_or_else(|| sub::node_display_name(&node));

            if test {
                if !runner::singbox_installed() {
                    eprintln!("sing-box not found, run `mole install` first.");
                    std::process::exit(1);
                }
                eprint!("  testing {node_name}... ");
                let r = bench::test_node_nosudo(&node);
                if !r.ok {
                    eprintln!("\x1b[31mfailed\x1b[0m");
                    std::process::exit(1);
                }
                if !r.ipv6 {
                    eprintln!("\x1b[31mno IPv6 support\x1b[0m");
                    std::process::exit(1);
                }
                println!("\x1b[32mok\x1b[0m ({}ms, {} v6)", r.latency_ms, r.ip);
            }

            let mut s = Store::load();
            s.add(node_name.clone(), raw);
            if let Err(e) = s.save() {
                eprintln!("error: {e}");
                std::process::exit(1);
            }
            println!("added and activated: {node_name}");
        }

        Commands::List {
            by_source,
            tree,
            level,
        } => {
            let s = Store::load();
            if s.nodes.is_empty() {
                println!("no nodes saved. use `mole add <uri>` or `mole sub add <url>`.");
                return;
            }

            let bench = store::load_bench();
            let v6_tag = |name: &str| -> &str {
                if bench.get(name).is_some_and(|b| b.ipv6) {
                    " \x1b[36mv6\x1b[0m"
                } else {
                    ""
                }
            };

            if tree {
                use std::collections::BTreeMap;
                let mut source_map: BTreeMap<Option<&str>, BTreeMap<String, Vec<&store::Node>>> =
                    BTreeMap::new();

                for n in &s.nodes {
                    let source = n.source.as_deref();
                    let country = n.name.split(" - ").next().unwrap_or(&n.name).to_string();
                    source_map
                        .entry(source)
                        .or_default()
                        .entry(country)
                        .or_default()
                        .push(n);
                }

                let depth = level.unwrap_or(3).min(3);

                for (source, country_map) in source_map {
                    let source_name = source.unwrap_or("manual");
                    println!("\n\x1b[1;36m{source_name}\x1b[0m");
                    println!("\x1b[2m{}\x1b[0m", "═".repeat(40));

                    if depth >= 2 {
                        for (country, nodes) in country_map {
                            println!("  \x1b[1;33m{country}\x1b[0m ({} nodes)", nodes.len());
                            if depth >= 3 {
                                for n in nodes {
                                    let marker = if n.active { ">" } else { " " };
                                    let v6 = v6_tag(&n.name);
                                    let name = n.name.split(" - ").nth(1).unwrap_or(&n.name);
                                    println!("    {marker}{v6} {name}");
                                }
                            }
                        }
                    } else {
                        let total: usize = country_map.values().map(|v| v.len()).sum();
                        println!("  {} total nodes", total);
                    }
                }
                println!();
            } else if by_source {
                // Group by source only
                use std::collections::BTreeMap;
                let mut groups: BTreeMap<Option<&str>, Vec<&store::Node>> = BTreeMap::new();
                for n in &s.nodes {
                    groups.entry(n.source.as_deref()).or_default().push(n);
                }

                // Print each group
                for (source, nodes) in groups {
                    let header = source.unwrap_or("manual");
                    println!("\n  \x1b[1;36m{}\x1b[0m ({} nodes)", header, nodes.len());
                    println!("  \x1b[2m{}\x1b[0m", "─".repeat(30));
                    for n in nodes {
                        let marker = if n.active { ">" } else { " " };
                        let v6 = v6_tag(&n.name);
                        println!("  {marker}{v6} {}", n.name);
                    }
                }
                println!();
            } else {
                // Simple flat list
                let width = s.nodes.len().to_string().len();
                for (i, n) in s.nodes.iter().enumerate() {
                    let marker = if n.active { ">" } else { " " };
                    let idx = i + 1;
                    let source = n
                        .source
                        .as_ref()
                        .map(|s| format!(" \x1b[2m[{s}]\x1b[0m"))
                        .unwrap_or_default();
                    let v6 = v6_tag(&n.name);
                    println!("{marker} \x1b[2m{idx:>width$}\x1b[0m{v6} {}{source}", n.name);
                }
            }
        }

        Commands::Use { name } => {
            let mut s = Store::load();
            match s.find_node(&name) {
                Ok(idx) => {
                    let node_name = s.nodes[idx].name.clone();
                    s.select_by_index(idx);
                    s.save().ok();
                    println!("active: {node_name}");
                }
                Err(candidates) if !candidates.is_empty() => {
                    eprintln!("ambiguous match for \"{name}\":");
                    for (i, cname) in &candidates {
                        eprintln!("  {} {cname}", i + 1);
                    }
                    eprintln!("be more specific or use the index number.");
                    std::process::exit(1);
                }
                _ => {
                    eprintln!("node not found: {name}");
                    std::process::exit(1);
                }
            }
        }

        Commands::Remove { name } => {
            let mut s = Store::load();
            match s.find_node(&name) {
                Ok(idx) => {
                    let removed = s.remove_by_index(idx);
                    s.save().ok();
                    println!("removed: {removed}");
                }
                Err(candidates) if !candidates.is_empty() => {
                    eprintln!("ambiguous match for \"{name}\":");
                    for (i, cname) in &candidates {
                        eprintln!("  {} {cname}", i + 1);
                    }
                    eprintln!("be more specific or use the index number.");
                    std::process::exit(1);
                }
                _ => {
                    eprintln!("node not found: {name}");
                    std::process::exit(1);
                }
            }
        }

        Commands::Rename => {
            let mut s = Store::load();
            s.normalize_names();
            if let Err(e) = s.save() {
                eprintln!("error: {e}");
                std::process::exit(1);
            }
            println!("node names normalized");
        }

        Commands::Qr { name } => {
            let s = Store::load();
            let node = match name {
                Some(n) => s.nodes.iter().find(|x| x.name == n),
                None => s.active_node(),
            };
            let node = match node {
                Some(n) => n,
                None => {
                    eprintln!("node not found. use `mole ls` to list nodes.");
                    std::process::exit(1);
                }
            };
            if let Ok(code) = qrcode::QrCode::new(node.uri.as_bytes()) {
                let string = code
                    .render::<char>()
                    .quiet_zone(false)
                    .max_dimensions(2, 2)
                    .build();
                println!();
                println!("  \x1b[1;36m  node: {}\x1b[0m", node.name);
                println!();
                for line in string.lines() {
                    println!("  {line}");
                }
                println!();
                println!("  \x1b[2mscan with v2rayNG/Kitsunebi/etc.\x1b[0m");
            } else {
                eprintln!("failed to generate QR code");
                std::process::exit(1);
            }
        }

        Commands::Config => {
            let s = Store::load();
            let node = match s.active_node() {
                Some(n) => n,
                None => {
                    eprintln!("no active node.");
                    std::process::exit(1);
                }
            };
            let parsed = match uri::ProxyNode::parse(&node.uri) {
                Ok(n) => n,
                Err(e) => {
                    eprintln!("error: {e}");
                    std::process::exit(1);
                }
            };
            println!("// {}", node.name);
            let cfg = config::generate(&[("proxy", &parsed)], &s.rules, s.bypass_cn, None, false);
            println!("{}", config::to_json_pretty(&cfg));
        }

        Commands::Test { all } => {
            let s = Store::load();
            if s.nodes.is_empty() {
                eprintln!("no nodes. use `mole add <uri>` or `mole sub add <url>` first.");
                std::process::exit(1);
            }
            if !runner::singbox_installed() {
                eprintln!("sing-box not found, run `mole install` first.");
                std::process::exit(1);
            }

            if all {
                let total = s.nodes.len();
                println!();
                println!("  \x1b[1mtest\x1b[0m  testing {} nodes (no sudo)...", total);
                println!("  \x1b[2m─────────────────────────────────────────────────\x1b[0m");

                let mut ok_count = 0;
                for (i, node) in s.nodes.iter().enumerate() {
                    let parsed = match uri::ProxyNode::parse(&node.uri) {
                        Ok(n) => n,
                        Err(e) => {
                            println!(
                                "  \x1b[31m✗\x1b[0m [{}/{}] {} — parse error: {e}",
                                i + 1,
                                total,
                                node.name
                            );
                            continue;
                        }
                    };
                    eprint!("  \x1b[33m…\x1b[0m [{}/{}] {} ", i + 1, total, node.name);
                    let r = bench::test_node_nosudo(&parsed);
                    eprint!("\r\x1b[K");
                    if r.ok {
                        ok_count += 1;
                        println!(
                            "  \x1b[32m✓\x1b[0m [{}/{}] {:<20} {:>5}ms  {}",
                            i + 1,
                            total,
                            node.name,
                            r.latency_ms,
                            r.ip
                        );
                    } else {
                        println!(
                            "  \x1b[31m✗\x1b[0m [{}/{}] {} — failed",
                            i + 1,
                            total,
                            node.name
                        );
                    }
                }

                println!("  \x1b[2m─────────────────────────────────────────────────\x1b[0m");
                println!("\n  {ok_count}/{total} nodes passed\n");
            } else {
                let node = match s.active_node() {
                    Some(n) => n,
                    None => {
                        eprintln!("no active node. use `mole use <name>`.");
                        std::process::exit(1);
                    }
                };
                let parsed = match uri::ProxyNode::parse(&node.uri) {
                    Ok(n) => n,
                    Err(e) => {
                        eprintln!("parse error: {e}");
                        std::process::exit(1);
                    }
                };
                eprint!("  testing {}... ", node.name);
                let r = bench::test_node_nosudo(&parsed);
                if r.ok {
                    println!("\x1b[32mok\x1b[0m ({}ms, {})", r.latency_ms, r.ip);
                } else {
                    eprintln!("\x1b[31mfailed\x1b[0m");
                    std::process::exit(1);
                }
            }
        }

        Commands::Bench { clean } => bench::run_bench(clean),
        Commands::Status => status::print_status(),

        Commands::Down => {
            // Kill orphaned mole processes (daemon, stuck bench, etc.)
            let my_pid = std::process::id().to_string();
            let mut killed_mole = false;
            if let Ok(output) = std::process::Command::new("pgrep")
                .args(["-f", "mole (up|bench)"])
                .output()
            {
                if output.status.success() {
                    let pids = String::from_utf8_lossy(&output.stdout);
                    for pid in pids.lines() {
                        let pid = pid.trim();
                        if pid != my_pid {
                            std::process::Command::new("kill").arg(pid).output().ok();
                            killed_mole = true;
                        }
                    }
                }
            }
            std::fs::remove_file(runner::pid_path()).ok();
            match runner::stop_singbox() {
                Ok(true) => println!("disconnected"),
                Ok(false) if killed_mole => println!("disconnected"),
                Ok(false) => println!("not running"),
                Err(e) => {
                    eprintln!("error: {e}");
                    std::process::exit(1);
                }
            }
        }

        // ── Subscriptions ───────────────────────────────────────
        Commands::Sub { action } => match action {
            SubCommands::Add { url, name } => {
                println!("fetching subscription...");
                let body = match sub::fetch(&url) {
                    Ok(b) => b,
                    Err(e) => {
                        eprintln!("error: {e}");
                        std::process::exit(1);
                    }
                };
                let nodes = sub::parse_nodes(&body);
                if nodes.is_empty() {
                    eprintln!("no valid nodes found in subscription");
                    std::process::exit(1);
                }
                let sub_name = name.unwrap_or_else(|| {
                    url.split("//")
                        .nth(1)
                        .and_then(|s| s.split('/').next())
                        .and_then(|s| s.split(':').next())
                        .unwrap_or("sub")
                        .to_string()
                });
                let total = nodes.len();
                println!("  {total} raw nodes, testing v6 ({}x parallel)...", bench::parallel_count());

                let mut s = Store::load();
                s.add_subscription(sub_name.clone(), url);

                if runner::singbox_installed() {
                    // Parallel v6 filter
                    let passing = bench::filter_v6_parallel(&nodes);
                    println!("  \x1b[32m{}\x1b[0m/{total} nodes support IPv6", passing.len());
                    let v6_nodes: Vec<(String, String)> = passing
                        .iter()
                        .map(|(name, _)| {
                            let uri = nodes.iter().find(|(n, _)| n == name).unwrap().1.clone();
                            (name.clone(), uri)
                        })
                        .collect();
                    s.update_subscription_nodes(&sub_name, v6_nodes);
                } else {
                    s.update_subscription_nodes(&sub_name, nodes);
                }

                if s.active_node().is_none() {
                    if let Some(first) = s.nodes.first() {
                        let name = first.name.clone();
                        s.select(&name);
                    }
                }
                s.save().ok();
                let final_count = s.nodes.iter().filter(|n| n.source.as_deref() == Some(&sub_name)).count();
                println!("added subscription: {sub_name} ({final_count} nodes)");
            }
            SubCommands::Update { test } => {
                let mut s = Store::load();
                if s.subscriptions.is_empty() {
                    println!("no subscriptions.");
                    return;
                }

                let can_test = test && runner::singbox_installed();
                if test && !can_test {
                    eprintln!("  \x1b[33mwarning\x1b[0m: sing-box not installed, skipping connectivity test");
                }

                let subs: Vec<_> = s.subscriptions.clone();
                for item in &subs {
                    eprint!("  updating {}... ", item.name);
                    match sub::fetch(&item.url) {
                        Ok(body) => {
                            let raw_nodes = sub::parse_nodes(&body);
                            eprintln!("{} raw nodes", raw_nodes.len());

                            let nodes = if can_test {
                                // Parallel v6 filter
                                let passing = bench::filter_v6_parallel(&raw_nodes);
                                eprintln!("  \x1b[32m{}\x1b[0m v6 nodes", passing.len());
                                passing
                                    .iter()
                                    .map(|(name, r)| {
                                        let uri = raw_nodes.iter().find(|(n, _)| n == name).unwrap().1.clone();
                                        let final_name = generate_node_name(&r.ip, &uri::ProxyNode::parse(&uri).unwrap());
                                        (final_name, uri)
                                    })
                                    .collect()
                            } else {
                                // Auto-name without testing
                                raw_nodes
                                    .iter()
                                    .filter_map(|(_, uri)| {
                                        uri::ProxyNode::parse(uri).ok().map(|parsed| {
                                            let ip = extract_ip_from_node(&parsed);
                                            (
                                                generate_node_name(&ip.unwrap_or_default(), &parsed),
                                                uri.clone(),
                                            )
                                        })
                                    })
                                    .collect()
                            };

                            s.update_subscription_nodes(
                                &item.name,
                                nodes,
                            );
                        }
                        Err(e) => eprintln!("failed: {e}"),
                    }
                }
                s.save().ok();
                println!("done");
            }
            SubCommands::Ls => {
                let s = Store::load();
                if s.subscriptions.is_empty() {
                    println!("no subscriptions.");
                    return;
                }
                for item in &s.subscriptions {
                    let count = s
                        .nodes
                        .iter()
                        .filter(|n| n.source.as_deref() == Some(&item.name))
                        .count();
                    let update = item.last_update.as_deref().unwrap_or("never");
                    println!("  {} — {} nodes (updated: {update})", item.name, count);
                }
            }
            SubCommands::Rm { name } => {
                let mut s = Store::load();
                if s.remove_subscription(&name) {
                    s.save().ok();
                    println!("removed: {name}");
                } else {
                    eprintln!("not found: {name}");
                    std::process::exit(1);
                }
            }
            SubCommands::Discover => {
                if !runner::singbox_installed() {
                    eprintln!("sing-box not found, run `mole install` first.");
                    std::process::exit(1);
                }

                let mut sources = store::load_sources();
                if sources.is_empty() {
                    // Fetch default sources from mole repo
                    eprint!("  fetching default sources... ");
                    match sub::fetch("https://raw.githubusercontent.com/LeonTing1010/mole/master/sources.json") {
                        Ok(body) => {
                            if let Ok(remote) = serde_json::from_str::<Vec<store::DiscoverSource>>(&body) {
                                sources = remote;
                                store::save_sources(&sources);
                                eprintln!("{} sources", sources.len());
                            } else {
                                eprintln!("parse error");
                                std::process::exit(1);
                            }
                        }
                        Err(e) => {
                            eprintln!("failed: {e}");
                            eprintln!("add sources manually: mole sub source add <url> [--name <name>] [--html]");
                            std::process::exit(1);
                        }
                    }
                }

                println!();
                println!("  \x1b[1mdiscover\x1b[0m  scanning {} sources for IPv6 nodes ({}x parallel)...",
                    sources.len(), bench::parallel_count());
                println!("  \x1b[2m─────────────────────────────────────────────────\x1b[0m");

                let mut s = Store::load();
                let mut all_sub_names: Vec<String> = Vec::new();
                let mut total_v6 = 0usize;
                const TARGET_NODES: usize = 5;

                for source in &sources {
                    if total_v6 >= TARGET_NODES * 2 {
                        break;
                    }

                    eprint!("  fetching {}... ", source.name);
                    let body = match sub::fetch(&source.url) {
                        Ok(b) => b,
                        Err(e) => { eprintln!("failed: {e}"); continue; }
                    };
                    let raw_nodes = if source.source_type == "html" {
                        sub::parse_nodes_from_html(&body)
                    } else {
                        sub::parse_nodes(&body)
                    };
                    if raw_nodes.is_empty() {
                        eprintln!("0 nodes");
                        continue;
                    }
                    eprintln!("{} nodes", raw_nodes.len());

                    let passing = bench::filter_v6_parallel(&raw_nodes);
                    let v6_count = passing.len();
                    total_v6 += v6_count;
                    all_sub_names.push(source.name.clone());

                    if v6_count > 0 {
                        s.add_subscription(source.name.clone(), source.url.clone());
                        let v6_nodes: Vec<(String, String)> = passing
                            .iter()
                            .map(|(pname, r)| {
                                let uri = raw_nodes.iter().find(|(n, _)| n == pname).unwrap().1.clone();
                                let final_name = generate_node_name(&r.ip, &uri::ProxyNode::parse(&uri).unwrap());
                                (final_name, uri)
                            })
                            .collect();
                        s.update_subscription_nodes(&source.name, v6_nodes);
                        println!("  \x1b[32m+{v6_count}\x1b[0m v6 nodes from {}", source.name);
                    } else {
                        println!("  \x1b[2m0 v6 nodes from {}\x1b[0m", source.name);
                    }
                }

                println!("  \x1b[2m─────────────────────────────────────────────────\x1b[0m");

                if total_v6 == 0 {
                    println!("\n  \x1b[31mno IPv6 nodes found\x1b[0m\n");
                } else {
                    s.save().ok();
                    println!("\n  \x1b[1m{total_v6}\x1b[0m v6 nodes found, benchmarking speed...");
                    println!("  \x1b[2m─────────────────────────────────────────────────\x1b[0m");

                    let all_v6: Vec<(String, String)> = s
                        .nodes
                        .iter()
                        .filter(|n| all_sub_names.iter().any(|sn| n.source.as_deref() == Some(sn.as_str())))
                        .map(|n| (n.name.clone(), n.uri.clone()))
                        .collect();

                    let ranked = bench::bench_speed_parallel(&all_v6);

                    println!("  \x1b[2m─────────────────────────────────────────────────\x1b[0m");

                    let keep: std::collections::HashSet<String> = ranked
                        .iter()
                        .take(TARGET_NODES)
                        .map(|(name, _)| name.clone())
                        .collect();

                    let to_remove: Vec<String> = s
                        .nodes
                        .iter()
                        .filter(|n| {
                            all_sub_names.iter().any(|sn| n.source.as_deref() == Some(sn.as_str()))
                                && !keep.contains(&n.name)
                        })
                        .map(|n| n.name.clone())
                        .collect();
                    for name in &to_remove {
                        s.remove(name);
                    }

                    if let Some((best_name, best_speed)) = ranked.first() {
                        s.select(best_name);
                        println!(
                            "\n  \x1b[1;32m★\x1b[0m fastest: \x1b[1m{best_name}\x1b[0m ({best_speed} KB/s)"
                        );
                    }
                    s.save().ok();

                    let kept = keep.len().min(ranked.len());
                    println!("  \x1b[2mkept top {kept} nodes. run `mole up` to connect.\x1b[0m\n");
                }
            }
            SubCommands::Source { action } => match action {
                SourceCommands::Add { url, name, html } => {
                    let source_name = name.unwrap_or_else(|| {
                        url.split("//")
                            .nth(1)
                            .and_then(|s| s.split('/').next())
                            .and_then(|s| s.split(':').next())
                            .unwrap_or("source")
                            .to_string()
                    });
                    let mut sources = store::load_sources();
                    // Replace if exists
                    sources.retain(|s| s.name != source_name);
                    sources.push(store::DiscoverSource {
                        name: source_name.clone(),
                        url,
                        source_type: if html { "html".into() } else { "subscription".into() },
                    });
                    store::save_sources(&sources);
                    println!("added source: {source_name}");
                }
                SourceCommands::Ls => {
                    let sources = store::load_sources();
                    if sources.is_empty() {
                        println!("no discover sources configured.");
                        println!("add sources with: mole sub source add <url> [--name <name>] [--html]");
                        return;
                    }
                    for s in &sources {
                        let tag = if s.source_type == "html" { " \x1b[2m[html]\x1b[0m" } else { "" };
                        println!("  {}{tag} — {}", s.name, s.url);
                    }
                }
                SourceCommands::Rm { name } => {
                    let mut sources = store::load_sources();
                    let before = sources.len();
                    sources.retain(|s| s.name != name);
                    if sources.len() < before {
                        store::save_sources(&sources);
                        println!("removed source: {name}");
                    } else {
                        eprintln!("not found: {name}");
                        std::process::exit(1);
                    }
                }
            },
        },

        // ── Rules ───────────────────────────────────────────────
        Commands::Rule { action } => match action {
            RuleCommands::Add {
                match_type,
                pattern,
                outbound,
            } => {
                let valid = [
                    "domain",
                    "domain_suffix",
                    "domain_keyword",
                    "ip_cidr",
                    "geoip",
                    "geosite",
                ];
                if !valid.contains(&match_type.as_str()) {
                    eprintln!("invalid type: {match_type}. valid: {}", valid.join(", "));
                    std::process::exit(1);
                }
                let mut s = Store::load();
                s.add_rule(match_type.clone(), pattern.clone(), outbound.clone());
                s.save().ok();
                println!("added: {match_type} {pattern} → {outbound}");
            }
            RuleCommands::Ls => {
                let s = Store::load();
                if s.rules.is_empty() {
                    println!("no custom rules.");
                    return;
                }
                for (i, r) in s.rules.iter().enumerate() {
                    println!("  [{i}] {} {} → {}", r.match_type, r.pattern, r.outbound);
                }
            }
            RuleCommands::Rm { index } => {
                let mut s = Store::load();
                if s.remove_rule(index) {
                    s.save().ok();
                    println!("removed rule #{index}");
                } else {
                    eprintln!("invalid index");
                    std::process::exit(1);
                }
            }
            RuleCommands::Clear => {
                let mut s = Store::load();
                s.clear_rules();
                s.save().ok();
                println!("all rules cleared");
            }
        },

        // ── Service ─────────────────────────────────────────────
        Commands::Service { action } => {
            let exe = std::env::current_exe()
                .expect("current exe")
                .to_str()
                .expect("utf8")
                .to_string();
            let home = dirs::home_dir().expect("home dir");
            let log = home.join(".mole/service.log");
            match action {
                ServiceCommands::Install => install_service(&exe, &home, &log),
                ServiceCommands::Uninstall => uninstall_service(&home),
            }
        }

        // ── Connect ─────────────────────────────────────────────
        Commands::Up {
            bypass_cn,
            daemon,
            strategy,
            share,
        } => {
            let is_daemon = std::env::var("MOLE_DAEMON").is_ok();
            let s = Store::load();

            if s.nodes.is_empty() {
                eprintln!("no nodes. use `mole add <uri>` or `mole sub add <url>` first.");
                std::process::exit(1);
            }
            if !runner::singbox_installed() {
                println!("sing-box not found, installing...");
                if let Err(e) = runner::install_singbox() {
                    eprintln!("install error: {e}");
                    std::process::exit(1);
                }
            }
            runner::stop_singbox().ok();

            // Daemon: re-exec in background
            if daemon && !is_daemon {
                let exe = std::env::current_exe().expect("current exe");
                let mut args = vec![
                    "up".to_string(),
                    "--bypass-cn".to_string(),
                    bypass_cn.to_string(),
                ];
                if let Some(ref strat) = strategy {
                    args.extend(["--strategy".to_string(), strat.clone()]);
                }
                if share {
                    args.push("--share".to_string());
                }
                let log = std::fs::File::create(runner::mole_dir().join("daemon.log"))
                    .expect("create log");
                let log_err = log.try_clone().expect("clone");
                let mut child = std::process::Command::new(&exe)
                    .args(&args)
                    .env("MOLE_DAEMON", "1")
                    .stdin(std::process::Stdio::null())
                    .stdout(log)
                    .stderr(log_err)
                    .spawn()
                    .expect("spawn daemon");
                let pid = child.id();
                // Reap child in background so parent doesn't leave a zombie
                std::thread::spawn(move || {
                    child.wait().ok();
                });
                std::fs::write(runner::pid_path(), pid.to_string()).ok();
                println!("mole running in background (pid={pid})");
                println!("use `mole down` to stop, `mole status` to check");
                return;
            }

            // Ctrl+C handler
            ctrlc::set_handler(move || {
                if runner::SHUTTING_DOWN.swap(true, Ordering::SeqCst) {
                    std::process::exit(130);
                }
                eprint!("\r\x1b[K  \x1b[2mstatus\x1b[0m  \x1b[33mdisconnecting...\x1b[0m");
                runner::stop_singbox().ok();
                eprintln!("\r\x1b[K  \x1b[2mstatus\x1b[0m  disconnected");
                std::fs::remove_file(runner::pid_path()).ok();
                std::process::exit(0);
            })
            .ok();

            if is_daemon {
                std::fs::write(runner::pid_path(), std::process::id().to_string()).ok();
            }

            if let Some(ref strat) = strategy {
                connect::run_strategy(&s, strat, bypass_cn, is_daemon, share);
            } else {
                connect::run_single(&s, bypass_cn, is_daemon, share);
            }

            std::fs::remove_file(runner::pid_path()).ok();
        }
    }
}

// ── Service helpers ─────────────────────────────────────────────────

fn install_service(exe: &str, home: &std::path::Path, log: &std::path::Path) {
    #[cfg(target_os = "macos")]
    {
        let dir = home.join("Library/LaunchAgents");
        std::fs::create_dir_all(&dir).ok();
        let path = dir.join("com.mole.vpn.plist");
        std::fs::write(&path, format!(r#"<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key><string>com.mole.vpn</string>
    <key>ProgramArguments</key><array><string>{exe}</string><string>up</string></array>
    <key>RunAtLoad</key><true/>
    <key>KeepAlive</key><true/>
    <key>StandardOutPath</key><string>{log}</string>
    <key>StandardErrorPath</key><string>{log}</string>
</dict>
</plist>"#, log = log.display())).expect("write plist");
        std::process::Command::new("launchctl")
            .args(["load", path.to_str().unwrap()])
            .output()
            .ok();
        println!("installed: {}", path.display());
    }
    #[cfg(target_os = "linux")]
    {
        let dir = home.join(".config/systemd/user");
        std::fs::create_dir_all(&dir).ok();
        let path = dir.join("mole.service");
        std::fs::write(
            &path,
            format!(
                r#"[Unit]
Description=Mole VPN
After=network-online.target
Wants=network-online.target
[Service]
Type=simple
ExecStart={exe} up
Restart=on-failure
RestartSec=5
StandardOutput=append:{log}
StandardError=append:{log}
[Install]
WantedBy=default.target"#,
                log = log.display()
            ),
        )
        .expect("write unit");
        std::process::Command::new("systemctl")
            .args(["--user", "enable", "--now", "mole"])
            .output()
            .ok();
        println!("installed: {}", path.display());
    }
    #[cfg(not(any(target_os = "macos", target_os = "linux")))]
    {
        let _ = (exe, home, log);
        eprintln!("unsupported platform");
    }
}

fn uninstall_service(home: &std::path::Path) {
    #[cfg(target_os = "macos")]
    {
        let path = home.join("Library/LaunchAgents/com.mole.vpn.plist");
        if path.exists() {
            std::process::Command::new("launchctl")
                .args(["unload", path.to_str().unwrap()])
                .output()
                .ok();
            std::fs::remove_file(&path).ok();
            println!("uninstalled");
        } else {
            println!("not installed");
        }
    }
    #[cfg(target_os = "linux")]
    {
        let path = home.join(".config/systemd/user/mole.service");
        if path.exists() {
            std::process::Command::new("systemctl")
                .args(["--user", "disable", "--now", "mole"])
                .output()
                .ok();
            std::fs::remove_file(&path).ok();
            println!("uninstalled");
        } else {
            println!("not installed");
        }
    }
    #[cfg(not(any(target_os = "macos", target_os = "linux")))]
    {
        let _ = home;
        eprintln!("unsupported platform");
    }
}

// Helper: generate a nice node name from IP and parsed node
fn generate_node_name(ip: &str, node: &uri::ProxyNode) -> String {
    let country = get_country_from_ip(ip);
    let proto = match node {
        uri::ProxyNode::Hysteria2 { .. } => "hy2",
        uri::ProxyNode::Hysteria { .. } => "hy",
        uri::ProxyNode::Vmess { .. } => "vmess",
        uri::ProxyNode::Vless { .. } => "vless",
        uri::ProxyNode::Trojan { .. } => "trojan",
        uri::ProxyNode::Shadowsocks { .. } => "ss",
        uri::ProxyNode::Tuic { .. } => "tuic",
        uri::ProxyNode::WireGuard { .. } => "wg",
    };

    // Extract last IP segment for uniqueness
    let ip_suffix = ip.rsplit('.').next().unwrap_or(ip);

    format!("{}-{}-{}", country, ip_suffix, proto)
}

// Helper: extract IP from parsed node (for naming)
fn extract_ip_from_node(node: &uri::ProxyNode) -> Option<String> {
    match node {
        uri::ProxyNode::Hysteria2 { host, .. } => Some(host.clone()),
        uri::ProxyNode::Hysteria { host, .. } => Some(host.clone()),
        uri::ProxyNode::Vmess { host, .. } => Some(host.clone()),
        uri::ProxyNode::Vless { host, .. } => Some(host.clone()),
        uri::ProxyNode::Trojan { host, .. } => Some(host.clone()),
        uri::ProxyNode::Shadowsocks { host, .. } => Some(host.clone()),
        uri::ProxyNode::Tuic { host, .. } => Some(host.clone()),
        uri::ProxyNode::WireGuard { host, .. } => Some(host.clone()),
    }
}

// Simple country code lookup (major CDNs/ISPs)
fn get_country_from_ip(ip: &str) -> &'static str {
    // This is a simplified version - in production you'd use a geo-ip database
    // For now, return common patterns
    if ip.starts_with("104.28")
        || ip.starts_with("104.16")
        || ip.starts_with("172.64")
        || ip.starts_with("172.65")
    {
        return "us"; // Cloudflare
    }
    if ip.starts_with("185.146.")
        || ip.starts_with("185.143.")
        || ip.starts_with("91.99")
        || ip.starts_with("85.198")
    {
        return "de"; // Germany
    }
    if ip.starts_with("5.175") || ip.starts_with("5.10") {
        return "de";
    }
    if ip.starts_with("91.132") || ip.starts_with("91.209") {
        return "ae"; // UAE
    }
    if ip.starts_with("103.160") || ip.starts_with("103.168") {
        return "ir"; // Iran
    }
    if ip.starts_with("104.26") || ip.starts_with("104.17") || ip.starts_with("104.254") {
        return "us";
    }
    if ip.starts_with("162.159") || ip.starts_with("162.120") {
        return "us";
    }
    "xx" // unknown
}
