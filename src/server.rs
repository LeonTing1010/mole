use crate::runner;
use crate::store::Store;
use serde::{Deserialize, Serialize};
use std::fs;
use std::io::{BufRead, BufReader, Read as _, Write as _};
use std::net::TcpListener;
use std::path::PathBuf;
use std::process::Command;
use std::thread;
use std::time::{Duration, Instant};

// ── Data ───────────────────────────────────────────────────────

const MAX_USERS: usize = 3;

#[derive(Serialize, Deserialize, Clone)]
pub struct ServerUser {
    pub name: String,
    pub password: String,
    pub created_at: String,
}

#[derive(Serialize, Deserialize, Clone)]
pub struct Server {
    pub name: String,
    pub instance_id: String,
    pub region: String,
    pub ip: String,
    #[serde(default)]
    pub ip_v6: String,
    pub created_at: String,
    pub hy2_port: u16,
    /// Legacy single-password field (kept for backward compat with existing servers.json)
    #[serde(default)]
    pub hy2_password: String,
    /// Multi-user slots (up to MAX_USERS)
    #[serde(default)]
    pub users: Vec<ServerUser>,
}

impl Server {
    pub fn has_slot(&self) -> bool {
        self.users.len() < MAX_USERS
    }

    pub fn user_count(&self) -> usize {
        if self.users.is_empty() && !self.hy2_password.is_empty() {
            1 // legacy single-user server
        } else {
            self.users.len()
        }
    }

    /// Build hy2 URI for a specific user on this server.
    pub fn user_uri(&self, password: &str, label: &str) -> String {
        let host = if self.ip.contains(':') {
            format!("[{}]", self.ip)
        } else {
            self.ip.clone()
        };
        format!(
            "hy2://{label}%3A{password}@{host}:{}?insecure=1&sni=bing.com#{label}",
            self.hy2_port
        )
    }
}

fn servers_path() -> PathBuf {
    runner::mole_dir().join("servers.json")
}

fn load_servers() -> Vec<Server> {
    let path = servers_path();
    if !path.exists() {
        return vec![];
    }
    let data = fs::read_to_string(&path).unwrap_or_default();
    serde_json::from_str(&data).unwrap_or_default()
}

fn save_servers(servers: &[Server]) {
    if let Ok(json) = serde_json::to_string_pretty(servers) {
        fs::write(servers_path(), json).ok();
    }
}

// ── Vultr API ──────────────────────────────────────────────────

const VULTR_API: &str = "https://api.vultr.com/v2";

fn api_key_path() -> PathBuf {
    runner::mole_dir().join("vultr_api_key")
}

fn vultr_api_key() -> Result<String, String> {
    if let Ok(key) = std::env::var("VULTR_API_KEY") {
        if !key.is_empty() {
            return Ok(key);
        }
    }
    let path = api_key_path();
    if path.exists() {
        let key = fs::read_to_string(&path)
            .map_err(|e| format!("read api key: {e}"))?
            .trim()
            .to_string();
        if !key.is_empty() {
            return Ok(key);
        }
    }
    Err("Vultr API key not found. Set VULTR_API_KEY env var or run: mole server setup".into())
}

fn vultr(
    api_key: &str,
    method: &str,
    path: &str,
    body: Option<&serde_json::Value>,
) -> Result<serde_json::Value, String> {
    let url = format!("{VULTR_API}{path}");
    let client = reqwest::blocking::Client::builder()
        .timeout(Duration::from_secs(30))
        .build()
        .map_err(|e| format!("http client: {e}"))?;

    let mut req = match method {
        "POST" => client.post(&url),
        "DELETE" => client.delete(&url),
        _ => client.get(&url),
    };
    req = req.header("Authorization", format!("Bearer {api_key}"));
    if let Some(b) = body {
        req = req.json(b);
    }

    let resp = req.send().map_err(|e| format!("API request: {e}"))?;
    let status = resp.status();
    let text = resp.text().map_err(|e| format!("read body: {e}"))?;

    if !status.is_success() {
        return Err(format!("Vultr API {status}: {text}"));
    }
    if text.is_empty() || status == reqwest::StatusCode::NO_CONTENT {
        return Ok(serde_json::Value::Null);
    }
    serde_json::from_str(&text).map_err(|e| format!("parse JSON: {e}"))
}

/// Create a Vultr instance. Returns (instance_id, ip).
/// Supports both IPv4 and IPv6-only plans.
fn create_instance(
    api_key: &str,
    region: &str,
    plan: &str,
    server_name: &str,
) -> Result<(String, String, String), String> {
    // Fetch SSH keys from Vultr account
    let ssh_keys: Vec<String> = vultr(api_key, "GET", "/ssh-keys", None)
        .ok()
        .and_then(|v| v["ssh_keys"].as_array().cloned())
        .unwrap_or_default()
        .iter()
        .filter_map(|k| k["id"].as_str().map(|s| s.to_string()))
        .collect();

    let mut body = serde_json::json!({
        "region": region,
        "plan": plan,
        "os_id": 2136,  // Debian 12 x64
        "label": server_name,
        "hostname": server_name,
        "backups": "disabled",
        "enable_ipv6": true,
    });
    if !ssh_keys.is_empty() {
        body["sshkey_id"] = serde_json::json!(ssh_keys);
    }

    let is_v6_only = plan.contains("v6");
    println!(
        "  Creating VPS ({plan}) in {region}{}...",
        if is_v6_only { " [IPv6-only]" } else { "" }
    );
    let resp = vultr(api_key, "POST", "/instances", Some(&body))?;
    let instance_id = resp["instance"]["id"]
        .as_str()
        .ok_or("missing instance id")?
        .to_string();

    println!("  Instance {instance_id} created, waiting for IP...");

    let start = Instant::now();
    loop {
        if start.elapsed() > Duration::from_secs(300) {
            return Err("timeout waiting for VPS (5min)".into());
        }
        thread::sleep(Duration::from_secs(5));

        let info = vultr(api_key, "GET", &format!("/instances/{instance_id}"), None)?;
        let status = info["instance"]["status"].as_str().unwrap_or("");

        // IPv6-only plans: main_ip is "0.0.0.0", use v6_main_ip instead
        let ip = if is_v6_only {
            info["instance"]["v6_main_ip"].as_str().unwrap_or("")
        } else {
            let v4 = info["instance"]["main_ip"].as_str().unwrap_or("");
            if !v4.is_empty() && v4 != "0.0.0.0" {
                v4
            } else {
                info["instance"]["v6_main_ip"].as_str().unwrap_or("")
            }
        };

        if status == "active" && !ip.is_empty() && ip != "0.0.0.0" && ip != "::" {
            println!("  VPS active: {ip}");
            let v6 = info["instance"]["v6_main_ip"]
                .as_str()
                .unwrap_or("")
                .to_string();
            return Ok((instance_id, ip.to_string(), v6));
        }
        eprint!(".");
    }
}

// ── SSH ────────────────────────────────────────────────────────

/// Use absolute path to avoid shell aliases hijacking ssh.
const SSH_BIN: &str = "/usr/bin/ssh";
const SSH_OPTS: [&str; 8] = [
    "-o",
    "StrictHostKeyChecking=no",
    "-o",
    "UserKnownHostsFile=/dev/null",
    "-o",
    "ConnectTimeout=10",
    "-o",
    "LogLevel=ERROR",
];

/// Format IP for SSH: IPv6 needs no brackets (ssh handles it), but user@ prefix works as-is.
fn ssh_host(ip: &str) -> String {
    format!("root@{ip}")
}

fn ssh_run(ip: &str, script: &str) -> Result<String, String> {
    let out = Command::new(SSH_BIN)
        .args(SSH_OPTS)
        .arg(ssh_host(ip))
        .arg(script)
        .output()
        .map_err(|e| format!("ssh exec: {e}"))?;
    if !out.status.success() {
        let stderr = String::from_utf8_lossy(&out.stderr);
        let stdout = String::from_utf8_lossy(&out.stdout);
        return Err(format!("remote command failed:\n{stderr}{stdout}"));
    }
    Ok(String::from_utf8_lossy(&out.stdout).to_string())
}

fn wait_for_ssh(ip: &str) -> Result<(), String> {
    println!("  Waiting for SSH...");
    let start = Instant::now();
    loop {
        if start.elapsed() > Duration::from_secs(180) {
            return Err("timeout waiting for SSH (3min)".into());
        }
        if let Ok(out) = Command::new(SSH_BIN)
            .args(SSH_OPTS)
            .arg(ssh_host(ip))
            .args(["echo", "ok"])
            .output()
        {
            if out.status.success() {
                println!("  SSH ready.");
                return Ok(());
            }
        }
        thread::sleep(Duration::from_secs(5));
        eprint!(".");
    }
}

// ── Deploy ─────────────────────────────────────────────────────

/// Generate Hysteria2 multi-user config YAML.
fn hy2_config_yaml(port: u16, passwords: &[(&str, &str)]) -> String {
    let mut config = format!(
        "listen: :{port}\n\
         \n\
         tls:\n\
         \x20 cert: /etc/hysteria/server.crt\n\
         \x20 key: /etc/hysteria/server.key\n\
         \n\
         auth:\n\
         \x20 type: userpass\n\
         \x20 userpass:\n"
    );
    for (user, pass) in passwords {
        config.push_str(&format!("\x20   {user}: {pass}\n"));
    }
    config.push_str(
        "\nmasquerade:\n\
         \x20 type: proxy\n\
         \x20 proxy:\n\
         \x20   url: https://bing.com\n\
         \x20   rewriteHost: true\n",
    );
    config
}

fn deploy_hysteria2(ip: &str, port: u16, passwords: &[(&str, &str)]) -> Result<(), String> {
    // 1. Install hysteria2 directly on VPS
    println!("  Installing Hysteria2...");
    ssh_run(ip, "curl -fsSL https://get.hy2.sh/ | bash")?;

    // 2. Generate self-signed TLS cert (no domain needed)
    println!("  Generating TLS certificate...");
    ssh_run(ip, "mkdir -p /etc/hysteria")?;
    ssh_run(
        ip,
        "openssl ecparam -genkey -name prime256v1 -out /etc/hysteria/server.key",
    )?;
    ssh_run(ip, "openssl req -new -x509 -key /etc/hysteria/server.key -out /etc/hysteria/server.crt -subj /CN=bing.com -days 36500")?;
    // get.hy2.sh runs service as 'hysteria' user — fix cert ownership
    ssh_run(ip, "chown hysteria:hysteria /etc/hysteria/server.key /etc/hysteria/server.crt 2>/dev/null; true")?;

    // 3. Write multi-user config
    println!("  Writing config ({} users)...", passwords.len());
    let config = hy2_config_yaml(port, passwords);
    ssh_run(
        ip,
        &format!("cat > /etc/hysteria/config.yaml << 'MOLE_EOF'\n{config}MOLE_EOF"),
    )?;

    // 4. Open firewall (some VPS have iptables/ufw by default)
    println!("  Configuring firewall...");
    let _ = ssh_run(
        ip,
        &format!(
        "if command -v ufw >/dev/null 2>&1; then ufw allow {port}/udp; ufw allow {port}/tcp; fi; \
         iptables -I INPUT -p udp --dport {port} -j ACCEPT 2>/dev/null; \
         iptables -I INPUT -p tcp --dport {port} -j ACCEPT 2>/dev/null; \
         true"
    ),
    );

    // 5. Enable and start
    println!("  Starting Hysteria2...");
    ssh_run(ip, "systemctl enable --now hysteria-server")?;

    // 6. Verify running
    thread::sleep(Duration::from_secs(2));
    let status = ssh_run(ip, "systemctl is-active hysteria-server")?;
    if status.trim() != "active" {
        let logs =
            ssh_run(ip, "journalctl -u hysteria-server -n 20 --no-pager").unwrap_or_default();
        return Err(format!(
            "hysteria-server not active: {}\n{logs}",
            status.trim()
        ));
    }

    println!("  Hysteria2 running.");
    Ok(())
}

/// Update Hysteria2 config on a running server with new user list, then restart.
fn update_hy2_users(ip: &str, port: u16, passwords: &[(&str, &str)]) -> Result<(), String> {
    let config = hy2_config_yaml(port, passwords);
    ssh_run(
        ip,
        &format!("cat > /etc/hysteria/config.yaml << 'MOLE_EOF'\n{config}MOLE_EOF"),
    )?;
    ssh_run(ip, "systemctl restart hysteria-server")?;
    thread::sleep(Duration::from_secs(2));
    let status = ssh_run(ip, "systemctl is-active hysteria-server")?;
    if status.trim() != "active" {
        return Err(format!(
            "hysteria-server not active after update: {}",
            status.trim()
        ));
    }
    Ok(())
}

// ── Random ─────────────────────────────────────────────────────

fn random_hex(len: usize) -> String {
    let bytes = len / 2;
    let mut buf = vec![0u8; bytes];
    if let Ok(mut f) = fs::File::open("/dev/urandom") {
        let _ = f.read_exact(&mut buf);
    }
    buf.iter().map(|b| format!("{b:02x}")).collect()
}

// ── Commands ───────────────────────────────────────────────────

pub fn cmd_setup() -> Result<(), String> {
    println!("Enter your Vultr API key (https://my.vultr.com/settings/#settingsapi):");
    let mut key = String::new();
    std::io::stdin()
        .read_line(&mut key)
        .map_err(|e| format!("read input: {e}"))?;
    let key = key.trim();
    if key.is_empty() {
        return Err("empty API key".into());
    }

    print!("Verifying... ");
    std::io::Write::flush(&mut std::io::stdout()).ok();
    vultr(key, "GET", "/account", None)?;
    println!("OK");

    fs::write(api_key_path(), key).map_err(|e| format!("save key: {e}"))?;
    println!("API key saved.");
    Ok(())
}

pub fn cmd_deploy(region: &str, plan: Option<&str>, name: Option<&str>) -> Result<(), String> {
    let api_key = vultr_api_key()?;
    let plan = plan.unwrap_or("vc2-1c-1gb");
    let user_name = name
        .map(|s| s.to_string())
        .unwrap_or_else(|| format!("user-{}", random_hex(4)));
    let password = random_hex(16);
    let port: u16 = 443;
    let now = chrono::Local::now().format("%Y-%m-%d %H:%M:%S").to_string();

    let mut servers = load_servers();

    // Try to find an existing server in the same region with an open slot
    let existing = servers
        .iter()
        .position(|s| s.region == region && s.has_slot());

    if let Some(idx) = existing {
        println!(
            "  Adding user '{user_name}' to existing server: {} ({}/{})",
            servers[idx].name,
            servers[idx].user_count() + 1,
            MAX_USERS
        );

        servers[idx].users.push(ServerUser {
            name: user_name.clone(),
            password: password.clone(),
            created_at: now,
        });

        // Rebuild password list and update remote config
        {
            let server = &servers[idx];
            let pw_list: Vec<(&str, &str)> = server
                .users
                .iter()
                .map(|u| (u.name.as_str(), u.password.as_str()))
                .collect();
            update_hy2_users(&server.ip, server.hy2_port, &pw_list)?;
        }

        let uri = servers[idx].user_uri(&password, &user_name);
        save_servers(&servers);

        println!("\n=== User Added ===");
        println!(
            "  Server: {} ({}/{})",
            servers[idx].name,
            servers[idx].user_count(),
            MAX_USERS
        );
        println!("  User:   {user_name}");
        println!("  URI:    {uri}");
    } else {
        // No available slot — create new VPS
        let server_name = format!("mole-{region}-{}", random_hex(6));
        println!("  No available slot in {region}, deploying new server: {server_name}");

        let (instance_id, ip, ip_v6) = create_instance(&api_key, region, plan, &server_name)?;

        let first_user = ServerUser {
            name: user_name.clone(),
            password: password.clone(),
            created_at: now.clone(),
        };
        let server = Server {
            name: server_name.clone(),
            instance_id,
            region: region.to_string(),
            ip: ip.clone(),
            ip_v6: ip_v6.clone(),
            created_at: now,
            hy2_port: port,
            hy2_password: String::new(),
            users: vec![first_user],
        };
        servers.push(server);
        save_servers(&servers);

        wait_for_ssh(&ip)?;

        let pw_list = [(&*user_name, &*password)];
        deploy_hysteria2(&ip, port, &pw_list)?;

        let server = servers.last().unwrap();
        let uri = server.user_uri(&password, &user_name);

        // Add node to mole for self-use
        let mut store = Store::load();
        let host = if ip.contains(':') {
            format!("[{ip}]")
        } else {
            ip.clone()
        };
        let node_name = format!("hy2-{host}:{port}");
        store.add(node_name.clone(), uri.clone());
        store.save()?;

        println!("\n=== Done ===");
        println!("  Server: {server_name} (1/{})", MAX_USERS);
        println!("  User:   {user_name}");
        println!("  URI:    {uri}");
    }

    Ok(())
}

/// Provision a full server: deploy VPS + fill all 3 user slots.
/// Outputs URIs ready to paste into Creem as license keys.
pub fn cmd_provision(region: &str, plan: Option<&str>) -> Result<(), String> {
    let api_key = vultr_api_key()?;
    let plan = plan.unwrap_or("vc2-1c-1gb");
    let server_name = format!("mole-{region}-{}", random_hex(6));
    let port: u16 = 443;
    let now = chrono::Local::now().format("%Y-%m-%d %H:%M:%S").to_string();

    println!(
        "Provisioning {server_name} with {} user slots...\n",
        MAX_USERS
    );

    let (instance_id, ip, ip_v6) = create_instance(&api_key, region, plan, &server_name)?;

    let mut users = Vec::new();
    let mut pw_list = Vec::new();
    for i in 1..=MAX_USERS {
        let name = format!("user-{i}");
        let password = random_hex(16);
        pw_list.push((name.clone(), password.clone()));
        users.push(ServerUser {
            name,
            password,
            created_at: now.clone(),
        });
    }

    let server = Server {
        name: server_name.clone(),
        instance_id,
        region: region.to_string(),
        ip: ip.clone(),
        ip_v6: ip_v6.clone(),
        created_at: now,
        hy2_port: port,
        hy2_password: String::new(),
        users: users.clone(),
    };
    let mut servers = load_servers();
    servers.push(server.clone());
    save_servers(&servers);

    wait_for_ssh(&ip)?;

    let refs: Vec<(&str, &str)> = pw_list
        .iter()
        .map(|(n, p)| (n.as_str(), p.as_str()))
        .collect();
    deploy_hysteria2(&ip, port, &refs)?;

    println!(
        "\n=== {} ready ({}/{}) ===\n",
        server_name, MAX_USERS, MAX_USERS
    );
    println!("Paste these into Creem as license keys:\n");
    println!("─────────────────────────────────────────");
    for u in &users {
        let uri = server.user_uri(&u.password, &u.name);
        println!("{uri}");
    }
    println!("─────────────────────────────────────────");
    println!("\n{} URIs ready. Each one = 1 paying user.", MAX_USERS);
    Ok(())
}

/// Add a user to an existing server by server name.
pub fn cmd_add_user(server_name: &str, user_name: Option<&str>) -> Result<(), String> {
    let mut servers = load_servers();
    let idx = servers
        .iter()
        .position(|s| s.name == server_name)
        .ok_or_else(|| format!("server '{server_name}' not found"))?;

    if !servers[idx].has_slot() {
        return Err(format!(
            "server '{server_name}' is full ({}/{})",
            MAX_USERS, MAX_USERS
        ));
    }

    let user_name = user_name
        .map(|s| s.to_string())
        .unwrap_or_else(|| format!("user-{}", random_hex(4)));
    let password = random_hex(16);

    servers[idx].users.push(ServerUser {
        name: user_name.clone(),
        password: password.clone(),
        created_at: chrono::Local::now().format("%Y-%m-%d %H:%M:%S").to_string(),
    });

    {
        let server = &servers[idx];
        let pw_list: Vec<(&str, &str)> = server
            .users
            .iter()
            .map(|u| (u.name.as_str(), u.password.as_str()))
            .collect();
        println!("  Updating config on {}...", server.name);
        update_hy2_users(&server.ip, server.hy2_port, &pw_list)?;
    }

    let server = &servers[idx];
    let uri = server.user_uri(&password, &user_name);
    save_servers(&servers);

    println!("\n=== User Added ===");
    println!(
        "  Server: {} ({}/{})",
        server.name,
        server.user_count(),
        MAX_USERS
    );
    println!("  User:   {user_name}");
    println!("  URI:    {uri}");
    Ok(())
}

/// Remove a user from a server.
pub fn cmd_rm_user(server_name: &str, user_name: &str) -> Result<(), String> {
    let mut servers = load_servers();
    let idx = servers
        .iter()
        .position(|s| s.name == server_name)
        .ok_or_else(|| format!("server '{server_name}' not found"))?;

    let before = servers[idx].users.len();
    servers[idx].users.retain(|u| u.name != user_name);
    if servers[idx].users.len() == before {
        return Err(format!("user '{user_name}' not found on '{server_name}'"));
    }

    if servers[idx].users.is_empty() {
        println!("  Last user removed. Destroy the server with: mole server destroy {server_name}");
        save_servers(&servers);
        return Ok(());
    }

    {
        let server = &servers[idx];
        let pw_list: Vec<(&str, &str)> = server
            .users
            .iter()
            .map(|u| (u.name.as_str(), u.password.as_str()))
            .collect();
        println!("  Updating config on {}...", server.name);
        update_hy2_users(&server.ip, server.hy2_port, &pw_list)?;
    }
    save_servers(&servers);

    println!(
        "  User '{user_name}' removed. ({}/{})",
        servers[idx].user_count(),
        MAX_USERS
    );
    Ok(())
}

pub fn cmd_ls() {
    let servers = load_servers();
    if servers.is_empty() {
        println!("No servers. Use 'mole server deploy' to create one.");
        return;
    }
    println!(
        "{:<20} {:<6} {:<16} {:<8} {}",
        "NAME", "REGION", "IP", "USERS", "CREATED"
    );
    for s in &servers {
        println!(
            "{:<20} {:<6} {:<16} {}/{:<5} {}",
            s.name,
            s.region,
            s.ip,
            s.user_count(),
            MAX_USERS,
            s.created_at
        );
        for u in &s.users {
            println!("  └─ {:<16} {}", u.name, u.created_at);
        }
    }
}

pub fn cmd_destroy(name: &str) -> Result<(), String> {
    let mut servers = load_servers();
    let idx = servers
        .iter()
        .position(|s| s.name == name)
        .ok_or_else(|| format!("server '{name}' not found"))?;
    let server = &servers[idx];
    let api_key = vultr_api_key()?;

    println!("Destroying: {} ({})", server.name, server.ip);
    vultr(
        &api_key,
        "DELETE",
        &format!("/instances/{}", server.instance_id),
        None,
    )?;
    println!("  VPS destroyed.");

    let host = if server.ip.contains(':') {
        format!("[{}]", server.ip)
    } else {
        server.ip.clone()
    };
    let node_name = format!("hy2-{}:{}", host, server.hy2_port);
    servers.remove(idx);
    save_servers(&servers);

    let mut store = Store::load();
    if store.remove(&node_name) {
        store.save()?;
        println!("  Node removed.");
    }
    println!("Done.");
    Ok(())
}

pub fn cmd_ssh_test(ip: &str) -> Result<(), String> {
    println!("Testing SSH to {ip}...");
    let out = ssh_run(
        ip,
        "echo 'mole-ok'; uname -a; free -h 2>/dev/null || vm_stat",
    )?;
    println!("{out}");
    Ok(())
}

pub fn cmd_regions() -> Result<(), String> {
    let api_key = vultr_api_key()?;
    let resp = vultr(&api_key, "GET", "/regions", None)?;
    let regions = resp["regions"].as_array().ok_or("unexpected response")?;

    println!("{:<6} {:<25} {}", "ID", "CITY", "CONTINENT");
    for r in regions {
        println!(
            "{:<6} {:<25} {}",
            r["id"].as_str().unwrap_or(""),
            r["city"].as_str().unwrap_or(""),
            r["continent"].as_str().unwrap_or("")
        );
    }
    Ok(())
}

// ── Creem Integration ─────────────────────────────────────────

#[derive(Serialize, Deserialize, Clone, Default)]
pub struct CreemConfig {
    pub api_key: String,
    #[serde(default)]
    pub default_region: String,
}

fn creem_config_path() -> PathBuf {
    runner::mole_dir().join("creem.json")
}

fn load_creem_config() -> Option<CreemConfig> {
    let path = creem_config_path();
    if !path.exists() {
        return None;
    }
    let data = fs::read_to_string(&path).ok()?;
    serde_json::from_str(&data).ok()
}

fn save_creem_config(config: &CreemConfig) -> Result<(), String> {
    let json = serde_json::to_string_pretty(config).map_err(|e| format!("{e}"))?;
    fs::write(creem_config_path(), json).map_err(|e| format!("save creem config: {e}"))
}

pub fn cmd_creem_setup() -> Result<(), String> {
    let mut config = load_creem_config().unwrap_or_default();

    println!("Creem API key (from https://app.creem.io/settings/developers):");
    let mut input = String::new();
    std::io::stdin()
        .read_line(&mut input)
        .map_err(|e| format!("{e}"))?;
    config.api_key = input.trim().to_string();

    if config.default_region.is_empty() {
        config.default_region = "nrt".to_string();
    }

    // Verify key works
    print!("Verifying... ");
    std::io::Write::flush(&mut std::io::stdout()).ok();
    let resp = reqwest::blocking::Client::builder()
        .timeout(Duration::from_secs(10))
        .build()
        .map_err(|e| format!("{e}"))?
        .get("https://api.creem.io/v1/products")
        .header("x-api-key", &config.api_key)
        .send()
        .map_err(|e| format!("verify: {e}"))?;
    if !resp.status().is_success() {
        return Err("invalid API key".into());
    }
    println!("OK");

    save_creem_config(&config)?;
    println!("Saved to {:?}", creem_config_path());
    Ok(())
}

/// Parse an HTTP request from a TcpStream. Returns (method, path, headers, body).
fn parse_http_request(
    stream: &mut std::net::TcpStream,
) -> Option<(String, String, Vec<(String, String)>, Vec<u8>)> {
    let mut reader = BufReader::new(stream.try_clone().ok()?);

    let mut request_line = String::new();
    reader.read_line(&mut request_line).ok()?;
    let parts: Vec<&str> = request_line.trim().splitn(3, ' ').collect();
    if parts.len() < 2 {
        return None;
    }
    let method = parts[0].to_string();
    let path = parts[1].to_string();

    let mut headers = Vec::new();
    let mut content_length = 0usize;
    loop {
        let mut line = String::new();
        reader.read_line(&mut line).ok()?;
        let line = line.trim_end().to_string();
        if line.is_empty() {
            break;
        }
        if let Some((key, value)) = line.split_once(':') {
            let key = key.trim().to_lowercase();
            let value = value.trim().to_string();
            if key == "content-length" {
                content_length = value.parse().unwrap_or(0);
            }
            headers.push((key, value));
        }
    }

    let mut body = vec![0u8; content_length];
    if content_length > 0 {
        reader.read_exact(&mut body).ok()?;
    }
    Some((method, path, headers, body))
}

fn http_response(stream: &mut std::net::TcpStream, status: u16, content_type: &str, body: &str) {
    let status_text = match status {
        200 => "OK",
        400 => "Bad Request",
        402 => "Payment Required",
        404 => "Not Found",
        500 => "Internal Server Error",
        503 => "Service Unavailable",
        _ => "OK",
    };
    let resp = format!(
        "HTTP/1.1 {status} {status_text}\r\n\
         Content-Type: {content_type}\r\n\
         Content-Length: {}\r\n\
         Connection: close\r\n\
         \r\n\
         {body}",
        body.len()
    );
    stream.write_all(resp.as_bytes()).ok();
    stream.flush().ok();
}

/// Parse query string into key-value pairs.
fn parse_query(query: &str) -> Vec<(String, String)> {
    query
        .split('&')
        .filter_map(|pair| {
            let (k, v) = pair.split_once('=')?;
            Some((
                urlencoding::decode(k).unwrap_or_default().into_owned(),
                urlencoding::decode(v).unwrap_or_default().into_owned(),
            ))
        })
        .collect()
}

// ── Replay Protection (persistent) ────────────────────────────

fn used_checkouts_path() -> PathBuf {
    runner::mole_dir().join("used_checkouts.json")
}

fn load_used_checkouts() -> std::collections::HashSet<String> {
    let path = used_checkouts_path();
    if !path.exists() {
        return std::collections::HashSet::new();
    }
    let data = fs::read_to_string(&path).unwrap_or_default();
    serde_json::from_str(&data).unwrap_or_default()
}

fn save_used_checkouts(set: &std::collections::HashSet<String>) {
    if let Ok(json) = serde_json::to_string(set) {
        fs::write(used_checkouts_path(), json).ok();
    }
}

/// Atomically check-and-mark a checkout ID as used. Returns false if already used.
fn mark_checkout_used(checkout_id: &str) -> bool {
    // File-based lock: load, check, insert, save — single-threaded via global mutex
    static LOCK: std::sync::Mutex<()> = std::sync::Mutex::new(());
    let _guard = LOCK.lock().unwrap();

    let mut set = load_used_checkouts();
    if set.contains(checkout_id) {
        return false;
    }
    set.insert(checkout_id.to_string());
    save_used_checkouts(&set);
    true
}

/// Verify checkout with Creem API. Returns customer email if paid.
fn verify_checkout(api_key: &str, checkout_id: &str) -> Result<String, String> {
    let resp = reqwest::blocking::Client::builder()
        .timeout(Duration::from_secs(10))
        .build()
        .map_err(|e| format!("{e}"))?
        .get(format!("https://api.creem.io/v1/checkouts/{checkout_id}"))
        .header("x-api-key", api_key)
        .send()
        .map_err(|e| format!("creem api: {e}"))?;

    if !resp.status().is_success() {
        return Err("invalid checkout".into());
    }

    let data: serde_json::Value = resp.json().map_err(|e| format!("{e}"))?;
    let status = data["status"].as_str().unwrap_or("");
    if status != "completed" {
        return Err(format!("checkout not completed: {status}"));
    }

    let email = data["customer"]["email"]
        .as_str()
        .or_else(|| data["customer"].as_str())
        .unwrap_or("customer")
        .to_string();
    Ok(email)
}

/// Allocate a user slot on an available server (or return error if none).
fn allocate_user(region: &str) -> Result<String, String> {
    let mut servers = load_servers();
    let idx = servers
        .iter()
        .position(|s| s.region == region && s.has_slot())
        .ok_or_else(|| format!("no available slots in {region}"))?;

    let password = random_hex(16);
    let user_name = format!("user-{}", random_hex(4));
    let now = chrono::Local::now().format("%Y-%m-%d %H:%M:%S").to_string();

    servers[idx].users.push(ServerUser {
        name: user_name.clone(),
        password: password.clone(),
        created_at: now,
    });

    {
        let server = &servers[idx];
        let pw_list: Vec<(&str, &str)> = server
            .users
            .iter()
            .map(|u| (u.name.as_str(), u.password.as_str()))
            .collect();
        println!(
            "[paid] adding to {}: {}/{}",
            server.name,
            server.users.len(),
            MAX_USERS
        );
        update_hy2_users(&server.ip, server.hy2_port, &pw_list)?;
    }

    let uri = servers[idx].user_uri(&password, &user_name);
    save_servers(&servers);
    Ok(uri)
}

/// HTML page shown to customer after successful payment.
fn success_html(uri: &str) -> String {
    format!(
        r#"<!DOCTYPE html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Your VPN is Ready</title>
<style>
  body {{ font-family: -apple-system, system-ui, sans-serif; max-width: 520px; margin: 40px auto; padding: 0 20px; background: #fafafa; }}
  .card {{ background: #fff; border-radius: 12px; padding: 32px; box-shadow: 0 2px 8px rgba(0,0,0,.08); }}
  h1 {{ color: #1a1a1a; font-size: 24px; margin: 0 0 8px; }}
  .subtitle {{ color: #666; margin: 0 0 24px; }}
  .uri-box {{ background: #f0f4ff; border: 1px solid #d0d8f0; border-radius: 8px; padding: 16px; word-break: break-all; font-family: monospace; font-size: 13px; position: relative; cursor: pointer; }}
  .uri-box:hover {{ background: #e8eeff; }}
  .copy-hint {{ text-align: center; color: #888; font-size: 12px; margin: 8px 0 24px; }}
  .steps {{ list-style: none; padding: 0; }}
  .steps li {{ padding: 10px 0; border-bottom: 1px solid #f0f0f0; }}
  .steps li:last-child {{ border: none; }}
  .platform {{ font-weight: 600; }}
  .done {{ text-align: center; color: #22c55e; font-size: 14px; display: none; margin: 8px 0; }}
</style></head><body>
<div class="card">
  <h1>Your VPN is ready!</h1>
  <p class="subtitle">Click the box below to copy your connection URI:</p>
  <div class="uri-box" onclick="copyURI()" id="uri">{uri}</div>
  <p class="copy-hint" id="hint">tap to copy</p>
  <p class="done" id="done">Copied!</p>
  <h3>How to connect</h3>
  <ul class="steps">
    <li><span class="platform">iPhone:</span> Shadowrocket → + → type: Hysteria2 → paste URI</li>
    <li><span class="platform">Android:</span> v2rayNG → + → import from clipboard</li>
    <li><span class="platform">Mac:</span> Clash Verge → Profiles → New → paste URI</li>
    <li><span class="platform">Windows:</span> v2rayN → Servers → paste URI</li>
  </ul>
</div>
<script>
function copyURI() {{
  navigator.clipboard.writeText(document.getElementById('uri').textContent);
  document.getElementById('hint').style.display='none';
  document.getElementById('done').style.display='block';
}}
</script>
</body></html>"#
    )
}

fn error_html(msg: &str) -> String {
    format!(
        r#"<!DOCTYPE html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Error</title>
<style>body {{ font-family: -apple-system, sans-serif; max-width: 520px; margin: 80px auto; text-align: center; color: #666; }}</style>
</head><body><h2>{msg}</h2><p>Please contact support.</p></body></html>"#
    )
}

/// Run the payment server.
/// Creem success_url should point to: http://<ip>:<port>/paid
pub fn cmd_serve(port: u16) -> Result<(), String> {
    let creem = load_creem_config().ok_or("Creem not configured. Run: mole server creem-setup")?;
    vultr_api_key()?;

    let addr = format!("0.0.0.0:{port}");
    let listener = TcpListener::bind(&addr).map_err(|e| format!("bind {addr}: {e}"))?;

    println!("mole serve on {addr}");
    println!();
    println!("  Set Creem success_url to:");
    println!("  http://<your-ip>:{port}/paid");
    println!();
    println!("  Creem checkout link example:");
    println!("  https://checkout.creem.io/... &success_url=http://<ip>:{port}/paid");

    for stream in listener.incoming() {
        let mut stream = match stream {
            Ok(s) => s,
            Err(_) => continue,
        };
        stream.set_read_timeout(Some(Duration::from_secs(30))).ok();

        let creem = creem.clone();
        thread::spawn(move || {
            let Some((method, path, _headers, _body)) = parse_http_request(&mut stream) else {
                return;
            };

            // Split path and query
            let (path_part, query_str) = path.split_once('?').unwrap_or((&path, ""));

            match (method.as_str(), path_part) {
                ("GET", "/health") => {
                    http_response(&mut stream, 200, "application/json", r#"{"status":"ok"}"#);
                }

                ("GET", "/paid") => {
                    let params = parse_query(query_str);
                    let checkout_id = params
                        .iter()
                        .find(|(k, _)| k == "checkout_id")
                        .map(|(_, v)| v.as_str())
                        .unwrap_or("");

                    if checkout_id.is_empty() {
                        http_response(
                            &mut stream,
                            400,
                            "text/html",
                            &error_html("Missing checkout ID"),
                        );
                        return;
                    }

                    // Atomic check-and-mark: prevents replay AND race condition
                    if !mark_checkout_used(checkout_id) {
                        http_response(
                            &mut stream,
                            400,
                            "text/html",
                            &error_html("This link has already been used"),
                        );
                        return;
                    }

                    // Verify with Creem API
                    println!("[paid] verifying checkout {checkout_id}...");
                    match verify_checkout(&creem.api_key, checkout_id) {
                        Err(e) => {
                            println!("[paid] ✗ {e}");
                            http_response(
                                &mut stream,
                                402,
                                "text/html",
                                &error_html("Payment not verified"),
                            );
                        }
                        Ok(email) => {
                            let region = params
                                .iter()
                                .find(|(k, _)| k == "region")
                                .map(|(_, v)| v.as_str())
                                .unwrap_or(if creem.default_region.is_empty() {
                                    "nrt"
                                } else {
                                    &creem.default_region
                                });

                            match allocate_user(region) {
                                Ok(uri) => {
                                    println!("[paid] ✓ {email} → {uri}");
                                    http_response(
                                        &mut stream,
                                        200,
                                        "text/html",
                                        &success_html(&uri),
                                    );
                                }
                                Err(e) => {
                                    println!("[paid] ✗ allocate failed: {e}");
                                    http_response(
                                        &mut stream,
                                        503,
                                        "text/html",
                                        &error_html(
                                            "No available servers. Please contact support.",
                                        ),
                                    );
                                }
                            }
                        }
                    }
                }

                _ => {
                    http_response(&mut stream, 404, "text/html", &error_html("Page not found"));
                }
            }
        });
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn random_hex_length() {
        assert_eq!(random_hex(6).len(), 6);
        assert_eq!(random_hex(16).len(), 16);
        assert_eq!(random_hex(32).len(), 32);
    }

    #[test]
    fn random_hex_unique() {
        let a = random_hex(16);
        let b = random_hex(16);
        assert_ne!(a, b);
    }

    #[test]
    fn random_hex_is_hex() {
        let h = random_hex(16);
        assert!(h.chars().all(|c| c.is_ascii_hexdigit()));
    }

    #[test]
    fn server_serialize_roundtrip() {
        let server = Server {
            name: "mole-nrt-abc123".into(),
            instance_id: "inst-12345".into(),
            region: "nrt".into(),
            ip: "1.2.3.4".into(),
            ip_v6: "2001:db8::1".into(),
            created_at: "2026-04-13 00:00:00".into(),
            hy2_port: 443,
            hy2_password: String::new(),
            users: vec![ServerUser {
                name: "alice".into(),
                password: "deadbeef".into(),
                created_at: "2026-04-13".into(),
            }],
        };
        let json = serde_json::to_string(&server).unwrap();
        let decoded: Server = serde_json::from_str(&json).unwrap();
        assert_eq!(decoded.name, "mole-nrt-abc123");
        assert_eq!(decoded.ip, "1.2.3.4");
        assert_eq!(decoded.hy2_port, 443);
        assert_eq!(decoded.users.len(), 1);
        assert_eq!(decoded.users[0].name, "alice");
    }

    #[test]
    fn server_has_slot() {
        let mut server = Server {
            name: "test".into(),
            instance_id: "i1".into(),
            region: "nrt".into(),
            ip: "1.1.1.1".into(),
            ip_v6: String::new(),
            created_at: "2026-01-01".into(),
            hy2_port: 443,
            hy2_password: String::new(),
            users: vec![],
        };
        assert!(server.has_slot());
        for i in 0..MAX_USERS {
            server.users.push(ServerUser {
                name: format!("u{i}"),
                password: format!("p{i}"),
                created_at: String::new(),
            });
        }
        assert!(!server.has_slot());
        assert_eq!(server.user_count(), MAX_USERS);
    }

    #[test]
    fn servers_list_serialize() {
        let servers = vec![
            Server {
                name: "s1".into(),
                instance_id: "i1".into(),
                region: "nrt".into(),
                ip: "1.1.1.1".into(),
                ip_v6: "".into(),
                created_at: "2026-01-01".into(),
                hy2_port: 443,
                hy2_password: String::new(),
                users: vec![],
            },
            Server {
                name: "s2".into(),
                instance_id: "i2".into(),
                region: "icn".into(),
                ip: "2.2.2.2".into(),
                ip_v6: "".into(),
                created_at: "2026-01-02".into(),
                hy2_port: 443,
                hy2_password: String::new(),
                users: vec![],
            },
        ];
        let json = serde_json::to_string_pretty(&servers).unwrap();
        let decoded: Vec<Server> = serde_json::from_str(&json).unwrap();
        assert_eq!(decoded.len(), 2);
        assert_eq!(decoded[0].region, "nrt");
        assert_eq!(decoded[1].region, "icn");
    }

    #[test]
    fn hy2_config_multiuser() {
        let passwords = [("alice", "pw1"), ("bob", "pw2")];
        let config = hy2_config_yaml(443, &passwords);
        assert!(config.contains("type: userpass"));
        assert!(config.contains("alice: pw1"));
        assert!(config.contains("bob: pw2"));
    }

    #[test]
    fn user_uri_format() {
        let server = Server {
            name: "test".into(),
            instance_id: "i1".into(),
            region: "nrt".into(),
            ip: "1.2.3.4".into(),
            ip_v6: "".into(),
            created_at: "2026-01-01".into(),
            hy2_port: 443,
            hy2_password: String::new(),
            users: vec![],
        };
        let uri = server.user_uri("mypw", "alice");
        assert_eq!(uri, "hy2://alice%3Amypw@1.2.3.4:443?insecure=1&sni=bing.com#alice");
    }

    #[test]
    fn backward_compat_no_users_field() {
        // Old servers.json without "users" field should deserialize fine
        let json = r#"{
            "name": "old-server",
            "instance_id": "i1",
            "region": "nrt",
            "ip": "1.1.1.1",
            "created_at": "2025-01-01",
            "hy2_port": 443,
            "hy2_password": "oldpw"
        }"#;
        let server: Server = serde_json::from_str(json).unwrap();
        assert_eq!(server.hy2_password, "oldpw");
        assert!(server.users.is_empty());
        assert_eq!(server.user_count(), 1); // legacy single-user
    }

    #[test]
    fn hy2_uri_format() {
        let ip = "1.2.3.4";
        let port = 443;
        let password = "testpw123";
        let name = "mole-nrt-abc";
        let uri = format!("hy2://{password}@{ip}:{port}?insecure=1&sni=bing.com#{name}");
        assert_eq!(
            uri,
            "hy2://testpw123@1.2.3.4:443?insecure=1&sni=bing.com#mole-nrt-abc"
        );
        // Verify it parses as a valid hy2 URI
        let node = crate::uri::ProxyNode::parse(&uri).unwrap();
        assert_eq!(node.protocol(), "hysteria2");
        assert_eq!(node.server_addr(), "1.2.3.4:443");
    }

    #[test]
    fn node_name_from_server() {
        let ip = "45.76.100.1";
        let port = 443;
        let node_name = format!("hy2-{ip}:{port}");
        assert_eq!(node_name, "hy2-45.76.100.1:443");
    }

    #[test]
    fn vultr_api_key_from_env() {
        // With env var set
        std::env::set_var("VULTR_API_KEY", "test-key-123");
        let key = vultr_api_key().unwrap();
        assert_eq!(key, "test-key-123");
        std::env::remove_var("VULTR_API_KEY");
    }

    #[test]
    fn vultr_api_key_empty_env() {
        std::env::set_var("VULTR_API_KEY", "");
        // Should fall through to file check (which won't exist in test)
        let result = vultr_api_key();
        assert!(result.is_err());
        std::env::remove_var("VULTR_API_KEY");
    }
}
