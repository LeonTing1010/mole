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
#[command(name = "mole", version, about = "Multi-protocol TUN VPN client (powered by sing-box)")]
struct Cli {
    #[command(subcommand)]
    command: Commands,
}

#[derive(Subcommand)]
enum Commands {
    /// Add a node (hysteria2://, vmess://, vless://, trojan://, ss://)
    Add {
        uri: String,
        #[arg(short, long)]
        name: Option<String>,
    },
    /// Connect to the active node
    Up {
        #[arg(long, default_value_t = true, action = clap::ArgAction::Set)]
        bypass_cn: bool,
        #[arg(short, long)]
        daemon: bool,
        #[arg(long)]
        strategy: Option<String>,
    },
    /// Disconnect (stop sing-box)
    Down,
    /// List saved nodes
    #[command(name = "ls")]
    List,
    /// Switch active node
    Use { name: String },
    /// Remove a saved node
    #[command(name = "rm")]
    Remove { name: String },
    /// Benchmark all nodes and activate the fastest
    Bench,
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
    Update,
    /// List subscriptions
    Ls,
    /// Remove a subscription and its nodes
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

        Commands::Add { uri: raw, name } => {
            let node = match uri::ProxyNode::parse(&raw) {
                Ok(n) => n,
                Err(e) => {
                    eprintln!("error: {e}");
                    std::process::exit(1);
                }
            };
            let node_name = name
                .or_else(|| node.name().map(|s| s.to_string()))
                .unwrap_or_else(|| node.server_addr());
            let mut s = Store::load();
            s.add(node_name.clone(), raw);
            if let Err(e) = s.save() {
                eprintln!("error: {e}");
                std::process::exit(1);
            }
            println!("added and activated: {node_name}");
        }

        Commands::List => {
            let s = Store::load();
            if s.nodes.is_empty() {
                println!("no nodes saved. use `mole add <uri>` or `mole sub add <url>`.");
                return;
            }
            for n in &s.nodes {
                let marker = if n.active { ">" } else { " " };
                let source = n.source.as_ref().map(|s| format!(" \x1b[2m[{s}]\x1b[0m")).unwrap_or_default();
                println!("{marker} {}{source}", n.name);
            }
        }

        Commands::Use { name } => {
            let mut s = Store::load();
            if s.select(&name) {
                s.save().ok();
                println!("active: {name}");
            } else {
                eprintln!("node not found: {name}");
                std::process::exit(1);
            }
        }

        Commands::Remove { name } => {
            let mut s = Store::load();
            if s.remove(&name) {
                s.save().ok();
                println!("removed: {name}");
            } else {
                eprintln!("node not found: {name}");
                std::process::exit(1);
            }
        }

        Commands::Config => {
            let s = Store::load();
            let node = match s.active_node() {
                Some(n) => n,
                None => { eprintln!("no active node."); std::process::exit(1); }
            };
            let parsed = match uri::ProxyNode::parse(&node.uri) {
                Ok(n) => n,
                Err(e) => { eprintln!("error: {e}"); std::process::exit(1); }
            };
            println!("// {}", node.name);
            let cfg = config::generate(&[("proxy", &parsed)], &s.rules, s.bypass_cn, None);
            println!("{}", config::to_json_pretty(&cfg));
        }

        Commands::Bench => bench::run_bench(),
        Commands::Status => status::print_status(),

        Commands::Down => {
            let mut was_daemon = false;
            let pid_file = runner::pid_path();
            if pid_file.exists() {
                if let Ok(pid_str) = std::fs::read_to_string(&pid_file) {
                    std::process::Command::new("kill").arg(pid_str.trim()).output().ok();
                    was_daemon = true;
                }
                std::fs::remove_file(&pid_file).ok();
            }
            match runner::stop_singbox() {
                Ok(true) => println!("disconnected"),
                Ok(false) if was_daemon => println!("disconnected"),
                Ok(false) => println!("not running"),
                Err(e) => { eprintln!("error: {e}"); std::process::exit(1); }
            }
        }

        // ── Subscriptions ───────────────────────────────────────

        Commands::Sub { action } => match action {
            SubCommands::Add { url, name } => {
                println!("fetching subscription...");
                let body = match sub::fetch(&url) {
                    Ok(b) => b,
                    Err(e) => { eprintln!("error: {e}"); std::process::exit(1); }
                };
                let nodes = sub::parse_nodes(&body);
                if nodes.is_empty() {
                    eprintln!("no valid nodes found in subscription");
                    std::process::exit(1);
                }
                let sub_name = name.unwrap_or_else(|| {
                    url.split("//").nth(1).and_then(|s| s.split('/').next())
                        .and_then(|s| s.split(':').next()).unwrap_or("sub").to_string()
                });
                let count = nodes.len();
                let mut s = Store::load();
                s.add_subscription(sub_name.clone(), url);
                s.update_subscription_nodes(&sub_name, nodes);
                if s.active_node().is_none() {
                    if let Some(first) = s.nodes.first() {
                        let name = first.name.clone();
                        s.select(&name);
                    }
                }
                s.save().ok();
                println!("added subscription: {sub_name} ({count} nodes)");
            }
            SubCommands::Update => {
                let mut s = Store::load();
                if s.subscriptions.is_empty() {
                    println!("no subscriptions.");
                    return;
                }
                let subs: Vec<_> = s.subscriptions.clone();
                for item in &subs {
                    eprint!("  updating {}... ", item.name);
                    match sub::fetch(&item.url) {
                        Ok(body) => {
                            let nodes = sub::parse_nodes(&body);
                            eprintln!("{} nodes", nodes.len());
                            s.update_subscription_nodes(&item.name, nodes);
                        }
                        Err(e) => eprintln!("failed: {e}"),
                    }
                }
                s.save().ok();
                println!("done");
            }
            SubCommands::Ls => {
                let s = Store::load();
                if s.subscriptions.is_empty() { println!("no subscriptions."); return; }
                for item in &s.subscriptions {
                    let count = s.nodes.iter().filter(|n| n.source.as_deref() == Some(&item.name)).count();
                    let update = item.last_update.as_deref().unwrap_or("never");
                    println!("  {} — {} nodes (updated: {update})", item.name, count);
                }
            }
            SubCommands::Rm { name } => {
                let mut s = Store::load();
                if s.remove_subscription(&name) { s.save().ok(); println!("removed: {name}"); }
                else { eprintln!("not found: {name}"); std::process::exit(1); }
            }
        },

        // ── Rules ───────────────────────────────────────────────

        Commands::Rule { action } => match action {
            RuleCommands::Add { match_type, pattern, outbound } => {
                let valid = ["domain", "domain_suffix", "domain_keyword", "ip_cidr", "geoip", "geosite"];
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
                if s.rules.is_empty() { println!("no custom rules."); return; }
                for (i, r) in s.rules.iter().enumerate() {
                    println!("  [{i}] {} {} → {}", r.match_type, r.pattern, r.outbound);
                }
            }
            RuleCommands::Rm { index } => {
                let mut s = Store::load();
                if s.remove_rule(index) { s.save().ok(); println!("removed rule #{index}"); }
                else { eprintln!("invalid index"); std::process::exit(1); }
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
            let exe = std::env::current_exe().expect("current exe").to_str().expect("utf8").to_string();
            let home = dirs::home_dir().expect("home dir");
            let log = home.join(".mole/service.log");
            match action {
                ServiceCommands::Install => install_service(&exe, &home, &log),
                ServiceCommands::Uninstall => uninstall_service(&home),
            }
        }

        // ── Connect ─────────────────────────────────────────────

        Commands::Up { bypass_cn, daemon, strategy } => {
            let is_daemon = std::env::var("MOLE_DAEMON").is_ok();
            let s = Store::load();

            if s.nodes.is_empty() {
                eprintln!("no nodes. use `mole add <uri>` or `mole sub add <url>` first.");
                std::process::exit(1);
            }
            if !runner::singbox_installed() {
                println!("sing-box not found, installing...");
                if let Err(e) = runner::install_singbox() {
                    eprintln!("install error: {e}"); std::process::exit(1);
                }
            }
            runner::stop_singbox().ok();

            // Daemon: re-exec in background
            if daemon && !is_daemon {
                let exe = std::env::current_exe().expect("current exe");
                let mut args = vec!["up".to_string(), "--bypass-cn".to_string(), bypass_cn.to_string()];
                if let Some(ref strat) = strategy {
                    args.extend(["--strategy".to_string(), strat.clone()]);
                }
                let log = std::fs::File::create(runner::mole_dir().join("daemon.log")).expect("create log");
                let log_err = log.try_clone().expect("clone");
                let mut child = std::process::Command::new(&exe)
                    .args(&args).env("MOLE_DAEMON", "1")
                    .stdin(std::process::Stdio::null()).stdout(log).stderr(log_err)
                    .spawn().expect("spawn daemon");
                let pid = child.id();
                // Reap child in background so parent doesn't leave a zombie
                std::thread::spawn(move || { child.wait().ok(); });
                std::fs::write(runner::pid_path(), pid.to_string()).ok();
                println!("mole running in background (pid={pid})");
                println!("use `mole down` to stop, `mole status` to check");
                return;
            }

            // Ctrl+C handler
            ctrlc::set_handler(move || {
                if runner::SHUTTING_DOWN.swap(true, Ordering::SeqCst) { std::process::exit(130); }
                eprint!("\r\x1b[K  \x1b[2mstatus\x1b[0m  \x1b[33mdisconnecting...\x1b[0m");
                runner::stop_singbox().ok();
                eprintln!("\r\x1b[K  \x1b[2mstatus\x1b[0m  disconnected");
                std::fs::remove_file(runner::pid_path()).ok();
                std::process::exit(0);
            }).ok();

            if is_daemon {
                std::fs::write(runner::pid_path(), std::process::id().to_string()).ok();
            }

            if let Some(ref strat) = strategy {
                connect::run_strategy(&s, strat, bypass_cn, is_daemon);
            } else {
                connect::run_single(&s, bypass_cn, is_daemon);
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
        std::process::Command::new("launchctl").args(["load", path.to_str().unwrap()]).output().ok();
        println!("installed: {}", path.display());
    }
    #[cfg(target_os = "linux")]
    {
        let dir = home.join(".config/systemd/user");
        std::fs::create_dir_all(&dir).ok();
        let path = dir.join("mole.service");
        std::fs::write(&path, format!(r#"[Unit]
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
WantedBy=default.target"#, log = log.display())).expect("write unit");
        std::process::Command::new("systemctl").args(["--user", "enable", "--now", "mole"]).output().ok();
        println!("installed: {}", path.display());
    }
    #[cfg(not(any(target_os = "macos", target_os = "linux")))]
    { let _ = (exe, home, log); eprintln!("unsupported platform"); }
}

fn uninstall_service(home: &std::path::Path) {
    #[cfg(target_os = "macos")]
    {
        let path = home.join("Library/LaunchAgents/com.mole.vpn.plist");
        if path.exists() {
            std::process::Command::new("launchctl").args(["unload", path.to_str().unwrap()]).output().ok();
            std::fs::remove_file(&path).ok();
            println!("uninstalled");
        } else { println!("not installed"); }
    }
    #[cfg(target_os = "linux")]
    {
        let path = home.join(".config/systemd/user/mole.service");
        if path.exists() {
            std::process::Command::new("systemctl").args(["--user", "disable", "--now", "mole"]).output().ok();
            std::fs::remove_file(&path).ok();
            println!("uninstalled");
        } else { println!("not installed"); }
    }
    #[cfg(not(any(target_os = "macos", target_os = "linux")))]
    { let _ = home; eprintln!("unsupported platform"); }
}
