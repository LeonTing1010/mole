mod config;
mod runner;
mod store;
mod uri;

use clap::{Parser, Subcommand};
use store::Store;

#[derive(Parser)]
#[command(name = "mole", version, about = "Hysteria2 TUN VPN client for macOS (powered by sing-box)")]
struct Cli {
    #[command(subcommand)]
    command: Commands,
}

#[derive(Subcommand)]
enum Commands {
    /// Add a node from a hysteria2:// URI
    Add {
        /// hysteria2:// or hy2:// URI
        uri: String,
        /// Custom name (default: parsed from URI fragment)
        #[arg(short, long)]
        name: Option<String>,
    },
    /// Connect to the active node
    Up {
        /// Bypass China mainland traffic (default: true)
        #[arg(long, default_value_t = true, action = clap::ArgAction::Set)]
        bypass_cn: bool,
    },
    /// Disconnect (stop sing-box)
    Down,
    /// List saved nodes
    #[command(name = "ls")]
    List,
    /// Switch active node
    Use {
        /// Node name
        name: String,
    },
    /// Remove a saved node
    #[command(name = "rm")]
    Remove {
        /// Node name
        name: String,
    },
    /// Download the sing-box binary
    Install,
    /// Print generated sing-box config for the active node (dry run)
    Config,
}

fn main() {
    let cli = Cli::parse();

    match cli.command {
        Commands::Install => {
            if let Err(e) = runner::install_singbox() {
                eprintln!("error: {e}");
                std::process::exit(1);
            }
        }

        Commands::Add { uri: raw, name } => {
            let parsed = match uri::Hy2Uri::parse(&raw) {
                Ok(u) => u,
                Err(e) => {
                    eprintln!("error: {e}");
                    std::process::exit(1);
                }
            };

            let node_name = name
                .or(parsed.name.clone())
                .unwrap_or_else(|| format!("{}:{}", parsed.host, parsed.port));

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
                println!("no nodes saved. use `mole add <uri>` to add one.");
                return;
            }
            for n in &s.nodes {
                let marker = if n.active { ">" } else { " " };
                println!("{marker} {}", n.name);
            }
        }

        Commands::Use { name } => {
            let mut s = Store::load();
            if s.select(&name) {
                s.save().ok();
                println!("active: {name}");
            } else {
                eprintln!("node not found: {name}");
                eprintln!("use `mole ls` to see saved nodes");
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
                None => {
                    eprintln!("no active node. use `mole add <uri>` first.");
                    std::process::exit(1);
                }
            };
            let parsed = match uri::Hy2Uri::parse(&node.uri) {
                Ok(u) => u,
                Err(e) => {
                    eprintln!("error parsing saved URI: {e}");
                    std::process::exit(1);
                }
            };
            println!("// {}", node.name);
            let cfg = config::generate(&parsed, s.bypass_cn);
            println!("{}", config::to_json_pretty(&cfg));
        }

        Commands::Down => {
            match runner::stop_singbox() {
                Ok(true) => println!("disconnected"),
                Ok(false) => println!("not running"),
                Err(e) => {
                    eprintln!("error: {e}");
                    std::process::exit(1);
                }
            }
        }

        Commands::Up { bypass_cn } => {
            let s = Store::load();
            let node = match s.active_node() {
                Some(n) => n.clone(),
                None => {
                    eprintln!("no active node. use `mole add <uri>` first.");
                    std::process::exit(1);
                }
            };

            let parsed = match uri::Hy2Uri::parse(&node.uri) {
                Ok(u) => u,
                Err(e) => {
                    eprintln!("error: {e}");
                    std::process::exit(1);
                }
            };

            if !runner::singbox_installed() {
                println!("sing-box not found, installing...");
                if let Err(e) = runner::install_singbox() {
                    eprintln!("install error: {e}");
                    std::process::exit(1);
                }
            }

            let mode = if bypass_cn { "bypass-cn" } else { "global" };
            println!("connecting to {} ({}) [mode: {mode}]", node.name, parsed.server_addr());

            let cfg = config::generate(&parsed, bypass_cn);
            let json = config::to_json_pretty(&cfg);

            let config_path = match runner::write_config(&json) {
                Ok(p) => p,
                Err(e) => {
                    eprintln!("error: {e}");
                    std::process::exit(1);
                }
            };

            match runner::run_singbox(&config_path) {
                Ok(status) => {
                    if !status.success() {
                        std::process::exit(status.code().unwrap_or(1));
                    }
                }
                Err(e) => {
                    eprintln!("error: {e}");
                    std::process::exit(1);
                }
            }
        }
    }
}
