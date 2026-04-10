use serde_json::Value;
use url::Url;

#[derive(Debug, Clone)]
pub enum ProxyNode {
    Hysteria2 {
        name: Option<String>,
        host: String,
        port: u16,
        password: String,
        sni: Option<String>,
        insecure: bool,
        hop_ports: Option<String>,
        obfs: Option<String>, // salamander
        obfs_password: Option<String>,
        up_mbps: Option<u32>,
        down_mbps: Option<u32>,
    },
    Hysteria {
        name: Option<String>,
        host: String,
        port: u16,
        auth: Option<String>,
        sni: Option<String>,
        insecure: bool,
        up_mbps: u32,
        down_mbps: u32,
        obfs: Option<String>,
    },
    Vmess {
        name: Option<String>,
        host: String,
        port: u16,
        uuid: String,
        alter_id: u16,
        security: String,
        transport: String,
        ws_host: Option<String>,
        ws_path: Option<String>,
        tls: bool,
        sni: Option<String>,
        fingerprint: Option<String>,
    },
    Vless {
        name: Option<String>,
        host: String,
        port: u16,
        uuid: String,
        flow: Option<String>, // xtls-rprx-vision
        security: String,     // tls, reality, none
        sni: Option<String>,
        fingerprint: Option<String>,
        // Reality fields
        pbk: Option<String>, // public key
        sid: Option<String>, // short id
        // Transport
        transport: String, // tcp, ws, grpc, xhttp
        ws_path: Option<String>,
        ws_host: Option<String>,
        grpc_service: Option<String>,
    },
    Trojan {
        name: Option<String>,
        host: String,
        port: u16,
        password: String,
        sni: Option<String>,
        fingerprint: Option<String>,
        security: String, // tls, reality
        pbk: Option<String>,
        sid: Option<String>,
        transport: String,
        ws_path: Option<String>,
        ws_host: Option<String>,
    },
    Shadowsocks {
        name: Option<String>,
        host: String,
        port: u16,
        method: String,
        password: String,
        plugin: Option<String>,
        plugin_opts: Option<String>,
    },
    Tuic {
        name: Option<String>,
        host: String,
        port: u16,
        uuid: String,
        password: String,
        sni: Option<String>,
        insecure: bool,
        congestion_control: String,
        udp_relay_mode: Option<String>,
        alpn: Option<Vec<String>>,
    },
    WireGuard {
        name: Option<String>,
        host: String,
        port: u16,
        private_key: String,
        peer_public_key: Option<String>,
        pre_shared_key: Option<String>,
        local_address: Vec<String>,
        reserved: Option<Vec<u8>>,
        mtu: u16,
    },
}

impl ProxyNode {
    pub fn parse(input: &str) -> Result<Self, String> {
        if input.starts_with("hysteria2://") || input.starts_with("hy2://") {
            parse_hy2(input)
        } else if input.starts_with("hysteria://") {
            parse_hysteria(input)
        } else if input.starts_with("vmess://") {
            parse_vmess(input)
        } else if input.starts_with("vless://") {
            parse_vless(input)
        } else if input.starts_with("trojan://") {
            parse_trojan(input)
        } else if input.starts_with("ss://") {
            parse_ss(input)
        } else if input.starts_with("tuic://") {
            parse_tuic(input)
        } else if input.starts_with("wg://") || input.starts_with("wireguard://") {
            parse_wireguard(input)
        } else {
            Err(format!(
                "unsupported protocol: {}",
                input.split("://").next().unwrap_or("?")
            ))
        }
    }

    pub fn name(&self) -> Option<&str> {
        match self {
            ProxyNode::Hysteria2 { name, .. }
            | ProxyNode::Hysteria { name, .. }
            | ProxyNode::Vmess { name, .. }
            | ProxyNode::Vless { name, .. }
            | ProxyNode::Trojan { name, .. }
            | ProxyNode::Shadowsocks { name, .. }
            | ProxyNode::Tuic { name, .. }
            | ProxyNode::WireGuard { name, .. } => name.as_deref(),
        }
    }

    pub fn protocol(&self) -> &str {
        match self {
            ProxyNode::Hysteria2 { .. } => "hysteria2",
            ProxyNode::Hysteria { .. } => "hysteria",
            ProxyNode::Vmess { .. } => "vmess",
            ProxyNode::Vless { .. } => "vless",
            ProxyNode::Trojan { .. } => "trojan",
            ProxyNode::Shadowsocks { .. } => "ss",
            ProxyNode::Tuic { .. } => "tuic",
            ProxyNode::WireGuard { .. } => "wireguard",
        }
    }

    pub fn server_addr(&self) -> String {
        match self {
            ProxyNode::Hysteria2 {
                host,
                port,
                hop_ports,
                ..
            } => {
                if let Some(ref ports) = hop_ports {
                    format!("{host}:{ports}")
                } else {
                    format!("{host}:{port}")
                }
            }
            ProxyNode::Vmess { host, port, .. }
            | ProxyNode::Hysteria { host, port, .. }
            | ProxyNode::Vless { host, port, .. }
            | ProxyNode::Trojan { host, port, .. }
            | ProxyNode::Shadowsocks { host, port, .. }
            | ProxyNode::Tuic { host, port, .. }
            | ProxyNode::WireGuard { host, port, .. } => format!("{host}:{port}"),
        }
    }

    /// Whether this node uses sing-box endpoint (not outbound).
    pub fn is_endpoint(&self) -> bool {
        matches!(self, ProxyNode::WireGuard { .. })
    }

    /// Generate sing-box outbound/endpoint JSON
    pub fn to_outbound(&self, tag: &str) -> Value {
        match self {
            ProxyNode::Hysteria2 {
                host,
                port,
                password,
                sni,
                insecure,
                hop_ports,
                obfs,
                obfs_password,
                up_mbps,
                down_mbps,
                ..
            } => {
                let mut tls = serde_json::json!({ "enabled": true });
                if let Some(ref s) = sni {
                    tls["server_name"] = serde_json::json!(s);
                }
                if *insecure {
                    tls["insecure"] = serde_json::json!(true);
                }

                let mut out = serde_json::json!({
                    "type": "hysteria2",
                    "tag": tag,
                    "server": host,
                    "server_port": port,
                    "password": password,
                    "tls": tls
                });

                if let Some(ref hop) = hop_ports {
                    let ports: Vec<String> =
                        hop.split(',').map(|s| s.trim().replace('-', ":")).collect();
                    out["server_ports"] = serde_json::json!(ports);
                    out["hop_interval"] = serde_json::json!("30s");
                }

                if let Some(ref obfs_type) = obfs {
                    let mut obfs_obj = serde_json::json!({ "type": obfs_type });
                    if let Some(ref pwd) = obfs_password {
                        obfs_obj["password"] = serde_json::json!(pwd);
                    }
                    out["obfs"] = obfs_obj;
                }

                if let Some(up) = up_mbps {
                    out["up_mbps"] = serde_json::json!(up);
                }
                if let Some(down) = down_mbps {
                    out["down_mbps"] = serde_json::json!(down);
                }

                out
            }

            ProxyNode::Hysteria {
                host,
                port,
                auth,
                sni,
                insecure,
                up_mbps,
                down_mbps,
                obfs,
                ..
            } => {
                let mut tls = serde_json::json!({ "enabled": true });
                if let Some(ref s) = sni {
                    tls["server_name"] = serde_json::json!(s);
                }
                if *insecure {
                    tls["insecure"] = serde_json::json!(true);
                }

                let mut out = serde_json::json!({
                    "type": "hysteria",
                    "tag": tag,
                    "server": host,
                    "server_port": port,
                    "up_mbps": up_mbps,
                    "down_mbps": down_mbps,
                    "tls": tls
                });

                if let Some(ref a) = auth {
                    out["auth_str"] = serde_json::json!(a);
                }
                if let Some(ref o) = obfs {
                    out["obfs"] = serde_json::json!(o);
                }

                out
            }

            ProxyNode::Vmess {
                host,
                port,
                uuid,
                alter_id,
                security,
                transport,
                ws_host,
                ws_path,
                tls,
                sni,
                fingerprint,
                ..
            } => {
                let mut out = serde_json::json!({
                    "type": "vmess",
                    "tag": tag,
                    "server": host,
                    "server_port": port,
                    "uuid": uuid,
                    "alter_id": alter_id,
                    "security": security
                });

                build_transport(&mut out, transport, ws_path, ws_host, &None);

                if *tls {
                    let sni_val = sni.as_deref().unwrap_or(host);
                    let mut tls_obj = serde_json::json!({
                        "enabled": true,
                        "server_name": sni_val
                    });
                    // ALPN: h2 for HTTP/2 transport, http/1.1 for ws/others
                    let alpn = if transport == "h2" {
                        vec!["h2"]
                    } else {
                        vec!["h2", "http/1.1"]
                    };
                    tls_obj["alpn"] = serde_json::json!(alpn);
                    if let Some(ref fp) = fingerprint {
                        tls_obj["utls"] = serde_json::json!({ "enabled": true, "fingerprint": fp });
                    }
                    out["tls"] = tls_obj;
                }

                out
            }

            ProxyNode::Vless {
                host,
                port,
                uuid,
                flow,
                security,
                sni,
                fingerprint,
                pbk,
                sid,
                transport,
                ws_path,
                ws_host,
                grpc_service,
                ..
            } => {
                let mut out = serde_json::json!({
                    "type": "vless",
                    "tag": tag,
                    "server": host,
                    "server_port": port,
                    "uuid": uuid
                });

                if let Some(ref f) = flow {
                    if !f.is_empty() {
                        out["flow"] = serde_json::json!(f);
                    }
                }

                // TLS / Reality
                let sni_val = sni.as_deref().unwrap_or(host);
                if security == "reality" {
                    let mut tls = serde_json::json!({
                        "enabled": true,
                        "server_name": sni_val,
                        "reality": { "enabled": true }
                    });
                    if let Some(ref fp) = fingerprint {
                        tls["utls"] = serde_json::json!({ "enabled": true, "fingerprint": fp });
                    }
                    if let Some(ref k) = pbk {
                        tls["reality"]["public_key"] = serde_json::json!(k);
                    }
                    if let Some(ref s) = sid {
                        tls["reality"]["short_id"] = serde_json::json!(s);
                    }
                    out["tls"] = tls;
                } else if security == "tls" {
                    let mut tls = serde_json::json!({
                        "enabled": true,
                        "server_name": sni_val
                    });
                    if let Some(ref fp) = fingerprint {
                        tls["utls"] = serde_json::json!({ "enabled": true, "fingerprint": fp });
                    }
                    out["tls"] = tls;
                }

                build_transport(&mut out, transport, ws_path, ws_host, grpc_service);

                out
            }

            ProxyNode::Trojan {
                host,
                port,
                password,
                sni,
                fingerprint,
                security,
                pbk,
                sid,
                transport,
                ws_path,
                ws_host,
                ..
            } => {
                let mut out = serde_json::json!({
                    "type": "trojan",
                    "tag": tag,
                    "server": host,
                    "server_port": port,
                    "password": password
                });

                let sni_val = sni.as_deref().unwrap_or(host);
                if security == "reality" {
                    let mut tls = serde_json::json!({
                        "enabled": true,
                        "server_name": sni_val,
                        "reality": { "enabled": true }
                    });
                    if let Some(ref fp) = fingerprint {
                        tls["utls"] = serde_json::json!({ "enabled": true, "fingerprint": fp });
                    }
                    if let Some(ref k) = pbk {
                        tls["reality"]["public_key"] = serde_json::json!(k);
                    }
                    if let Some(ref s) = sid {
                        tls["reality"]["short_id"] = serde_json::json!(s);
                    }
                    out["tls"] = tls;
                } else if security != "none" {
                    // tls (default) — skip TLS when security=none
                    let mut tls = serde_json::json!({
                        "enabled": true,
                        "server_name": sni_val
                    });
                    if let Some(ref fp) = fingerprint {
                        tls["utls"] = serde_json::json!({ "enabled": true, "fingerprint": fp });
                    }
                    out["tls"] = tls;
                }

                build_transport(&mut out, transport, ws_path, ws_host, &None);

                out
            }

            ProxyNode::Shadowsocks {
                host,
                port,
                method,
                password,
                plugin,
                plugin_opts,
                ..
            } => {
                let mut out = serde_json::json!({
                    "type": "shadowsocks",
                    "tag": tag,
                    "server": host,
                    "server_port": port,
                    "method": method,
                    "password": password
                });
                if let Some(ref p) = plugin {
                    out["plugin"] = serde_json::json!(p);
                    if let Some(ref opts) = plugin_opts {
                        out["plugin_opts"] = serde_json::json!(opts);
                    }
                }
                out
            }

            ProxyNode::Tuic {
                host,
                port,
                uuid,
                password,
                sni,
                insecure,
                congestion_control,
                udp_relay_mode,
                alpn,
                ..
            } => {
                let mut tls = serde_json::json!({ "enabled": true });
                if let Some(ref s) = sni {
                    tls["server_name"] = serde_json::json!(s);
                }
                if *insecure {
                    tls["insecure"] = serde_json::json!(true);
                }
                if let Some(ref a) = alpn {
                    tls["alpn"] = serde_json::json!(a);
                } else {
                    tls["alpn"] = serde_json::json!(["h3"]);
                }

                let mut out = serde_json::json!({
                    "type": "tuic",
                    "tag": tag,
                    "server": host,
                    "server_port": port,
                    "uuid": uuid,
                    "password": password,
                    "congestion_control": congestion_control,
                    "tls": tls
                });

                if let Some(ref mode) = udp_relay_mode {
                    out["udp_relay_mode"] = serde_json::json!(mode);
                }

                out
            }

            ProxyNode::WireGuard {
                host,
                port,
                private_key,
                peer_public_key,
                pre_shared_key,
                local_address,
                reserved,
                mtu,
                ..
            } => {
                // sing-box 1.13+: WireGuard is an endpoint, not an outbound.
                // The tag is still used as outbound target in route rules.
                let mut peer = serde_json::json!({
                    "address": host,
                    "port": port,
                    "allowed_ips": ["0.0.0.0/0", "::/0"]
                });
                if let Some(ref pk) = peer_public_key {
                    peer["public_key"] = serde_json::json!(pk);
                }
                if let Some(ref psk) = pre_shared_key {
                    peer["pre_shared_key"] = serde_json::json!(psk);
                }
                if let Some(ref r) = reserved {
                    peer["reserved"] = serde_json::json!(r);
                }

                serde_json::json!({
                    "type": "wireguard",
                    "tag": tag,
                    "private_key": private_key,
                    "address": local_address,
                    "peers": [peer],
                    "mtu": mtu
                })
            }
        }
    }
}

fn build_transport(
    out: &mut Value,
    transport: &str,
    ws_path: &Option<String>,
    ws_host: &Option<String>,
    grpc_service: &Option<String>,
) {
    match transport {
        "ws" => {
            let mut t = serde_json::json!({ "type": "ws" });
            if let Some(ref p) = ws_path {
                t["path"] = serde_json::json!(p);
            }
            if let Some(ref h) = ws_host {
                t["headers"] = serde_json::json!({ "Host": h });
            }
            out["transport"] = t;
        }
        "grpc" => {
            let mut t = serde_json::json!({ "type": "grpc" });
            if let Some(ref s) = grpc_service {
                t["service_name"] = serde_json::json!(s);
            }
            out["transport"] = t;
        }
        "h2" | "http" => {
            let mut t = serde_json::json!({ "type": "http" });
            if let Some(ref p) = ws_path {
                t["path"] = serde_json::json!(p);
            }
            if let Some(ref h) = ws_host {
                t["host"] = serde_json::json!([h]);
            }
            out["transport"] = t;
        }
        "xhttp" | "splithttp" => {
            let mut t = serde_json::json!({ "type": "httpupgrade" });
            if let Some(ref p) = ws_path {
                t["path"] = serde_json::json!(p);
            }
            if let Some(ref h) = ws_host {
                t["host"] = serde_json::json!(h);
            }
            out["transport"] = t;
        }
        "quic" => {
            out["transport"] = serde_json::json!({ "type": "quic" });
        }
        _ => {} // tcp, kcp = no transport needed
    }
}

// --- Helpers ---

fn parse_url_based(input: &str, scheme: &str) -> Result<Url, String> {
    let fake = input.replacen(&format!("{scheme}://"), "https://", 1);
    Url::parse(&fake).map_err(|e| format!("invalid URI: {e}"))
}

/// Extract the full userinfo (username + optional :password) from a URL.
/// Needed because protocols like Trojan/Hysteria2 treat the entire userinfo
/// as a single credential, but `Url::username()` splits on `:`.
fn extract_userinfo(parsed: &Url) -> String {
    let user = parsed.username();
    match parsed.password() {
        Some(pw) => {
            let u = urlencoding::decode(user).unwrap_or_else(|_| user.into());
            let p = urlencoding::decode(pw).unwrap_or_else(|_| pw.into());
            format!("{u}:{p}")
        }
        None => urlencoding::decode(user)
            .unwrap_or_else(|_| user.into())
            .to_string(),
    }
}

fn get_query(parsed: &Url, key: &str) -> Option<String> {
    parsed
        .query_pairs()
        .find(|(k, _)| k == key)
        .map(|(_, v)| v.to_string())
        .filter(|v| !v.is_empty())
}

// --- Parsers ---

fn parse_hy2(input: &str) -> Result<ProxyNode, String> {
    let normalized = if input.starts_with("hy2://") {
        input.replacen("hy2://", "hysteria2://", 1)
    } else {
        input.to_string()
    };

    let fake = normalized.replacen("hysteria2://", "https://", 1);
    let parsed = Url::parse(&fake).map_err(|e| format!("invalid URI: {e}"))?;

    let auth = extract_userinfo(&parsed);
    if auth.is_empty() {
        return Err("missing auth (password) in URI".into());
    }

    let host = parsed.host_str().ok_or("missing host")?.to_string();
    let port = parsed.port().unwrap_or(443);

    let mut sni = None;
    let mut insecure = false;
    let mut hop_ports = None;
    let mut obfs = None;
    let mut obfs_password = None;
    let mut up_mbps = None;
    let mut down_mbps = None;

    for (k, v) in parsed.query_pairs() {
        match k.as_ref() {
            "sni" => sni = Some(v.to_string()),
            "insecure" => insecure = v == "1" || v == "true",
            "mport" => hop_ports = Some(v.to_string()),
            "obfs" => obfs = Some(v.to_string()),
            "obfs-password" => obfs_password = Some(v.to_string()),
            "up" | "upmbps" => up_mbps = v.parse().ok(),
            "down" | "downmbps" => down_mbps = v.parse().ok(),
            _ => {}
        }
    }

    let name = parsed.fragment().map(|f| {
        urlencoding::decode(f)
            .unwrap_or_else(|_| f.into())
            .to_string()
    });

    Ok(ProxyNode::Hysteria2 {
        name,
        host,
        port,
        password: auth,
        sni,
        insecure,
        hop_ports,
        obfs,
        obfs_password,
        up_mbps,
        down_mbps,
    })
}

fn parse_vmess(input: &str) -> Result<ProxyNode, String> {
    let b64 = input
        .strip_prefix("vmess://")
        .ok_or("missing vmess:// prefix")?;

    use base64::Engine;
    let decoded = base64::engine::general_purpose::STANDARD
        .decode(b64)
        .or_else(|_| base64::engine::general_purpose::STANDARD_NO_PAD.decode(b64))
        .map_err(|e| format!("base64 decode: {e}"))?;

    let json: Value = serde_json::from_slice(&decoded).map_err(|e| format!("JSON parse: {e}"))?;

    let host = json["add"]
        .as_str()
        .ok_or("missing 'add' field")?
        .to_string();
    let port: u16 = json["port"]
        .as_str()
        .and_then(|s| s.parse().ok())
        .or_else(|| json["port"].as_u64().map(|n| n as u16))
        .ok_or("missing/invalid 'port'")?;
    let uuid = json["id"].as_str().ok_or("missing 'id' field")?.to_string();
    let alter_id: u16 = json["aid"]
        .as_str()
        .and_then(|s| s.parse().ok())
        .or_else(|| json["aid"].as_u64().map(|n| n as u16))
        .unwrap_or(0);
    let security = json["scy"].as_str().unwrap_or("auto").to_string();
    let transport = json["net"].as_str().unwrap_or("tcp").to_string();
    let name = json["ps"].as_str().map(|s| s.to_string());
    let ws_host = json["host"]
        .as_str()
        .filter(|s| !s.is_empty())
        .map(|s| s.to_string());
    let ws_path = json["path"]
        .as_str()
        .filter(|s| !s.is_empty())
        .map(|s| s.to_string());
    let tls = json["tls"].as_str().unwrap_or("") == "tls";
    let sni = json["sni"]
        .as_str()
        .filter(|s| !s.is_empty())
        .map(|s| s.to_string());
    let fingerprint = json["fp"]
        .as_str()
        .filter(|s| !s.is_empty())
        .map(|s| s.to_string());

    Ok(ProxyNode::Vmess {
        name,
        host,
        port,
        uuid,
        alter_id,
        security,
        transport,
        ws_host,
        ws_path,
        tls,
        sni,
        fingerprint,
    })
}

fn parse_vless(input: &str) -> Result<ProxyNode, String> {
    let parsed = parse_url_based(input, "vless")?;

    let uuid = parsed.username().to_string();
    if uuid.is_empty() {
        return Err("missing UUID in vless URI".into());
    }

    let host = parsed.host_str().ok_or("missing host")?.to_string();
    let port = parsed.port().unwrap_or(443);

    let security = get_query(&parsed, "security").unwrap_or_else(|| "none".into());
    let sni = get_query(&parsed, "sni");
    let flow = get_query(&parsed, "flow");
    let fingerprint = get_query(&parsed, "fp");
    let pbk = get_query(&parsed, "pbk");
    let sid = get_query(&parsed, "sid");
    let transport = get_query(&parsed, "type").unwrap_or_else(|| "tcp".into());
    let ws_path = get_query(&parsed, "path");
    let ws_host = get_query(&parsed, "host");
    let grpc_service = get_query(&parsed, "serviceName");

    let name = parsed.fragment().map(|f| {
        urlencoding::decode(f)
            .unwrap_or_else(|_| f.into())
            .to_string()
    });

    Ok(ProxyNode::Vless {
        name,
        host,
        port,
        uuid,
        flow,
        security,
        sni,
        fingerprint,
        pbk,
        sid,
        transport,
        ws_path,
        ws_host,
        grpc_service,
    })
}

fn parse_trojan(input: &str) -> Result<ProxyNode, String> {
    let parsed = parse_url_based(input, "trojan")?;

    let password = extract_userinfo(&parsed);
    if password.is_empty() {
        return Err("missing password in trojan URI".into());
    }

    let host = parsed.host_str().ok_or("missing host")?.to_string();
    let port = parsed.port().unwrap_or(443);

    let sni = get_query(&parsed, "sni");
    let fingerprint = get_query(&parsed, "fp");
    let security = get_query(&parsed, "security").unwrap_or_else(|| "tls".into());
    let pbk = get_query(&parsed, "pbk");
    let sid = get_query(&parsed, "sid");
    let transport = get_query(&parsed, "type").unwrap_or_else(|| "tcp".into());
    let ws_path = get_query(&parsed, "path");
    let ws_host = get_query(&parsed, "host");

    let name = parsed.fragment().map(|f| {
        urlencoding::decode(f)
            .unwrap_or_else(|_| f.into())
            .to_string()
    });

    Ok(ProxyNode::Trojan {
        name,
        host,
        port,
        password,
        sni,
        fingerprint,
        security,
        pbk,
        sid,
        transport,
        ws_path,
        ws_host,
    })
}

fn parse_ss(input: &str) -> Result<ProxyNode, String> {
    use base64::Engine;

    let rest = input.strip_prefix("ss://").ok_or("missing ss:// prefix")?;

    // Format: ss://base64(method:password)@host:port?plugin=...&plugin-opts=...#name
    // Or:     ss://base64(method:password@host:port)#name
    let (encoded, fragment) = match rest.split_once('#') {
        Some((e, f)) => (
            e,
            Some(
                urlencoding::decode(f)
                    .unwrap_or_else(|_| f.into())
                    .to_string(),
            ),
        ),
        None => (rest, None),
    };

    // Try format: base64@host:port?query
    if let Some((b64, server_and_query)) = encoded.split_once('@') {
        let decoded = base64::engine::general_purpose::STANDARD
            .decode(b64)
            .or_else(|_| base64::engine::general_purpose::STANDARD_NO_PAD.decode(b64))
            .map_err(|e| format!("base64 decode: {e}"))?;
        let cred = String::from_utf8(decoded).map_err(|e| format!("utf8: {e}"))?;

        let (method, password) = cred.split_once(':').ok_or("invalid method:password")?;

        // Split server from query params
        let (server, query) = match server_and_query.split_once('?') {
            Some((s, q)) => (s, Some(q)),
            None => (server_and_query, None),
        };

        let (host, port) = parse_host_port(server)?;

        let (plugin, plugin_opts) = parse_ss_plugin(query);

        return Ok(ProxyNode::Shadowsocks {
            name: fragment,
            host,
            port,
            method: method.to_string(),
            password: password.to_string(),
            plugin,
            plugin_opts,
        });
    }

    // Try format: base64(method:password@host:port)
    let decoded = base64::engine::general_purpose::STANDARD
        .decode(encoded)
        .or_else(|_| base64::engine::general_purpose::STANDARD_NO_PAD.decode(encoded))
        .map_err(|e| format!("base64 decode: {e}"))?;
    let full = String::from_utf8(decoded).map_err(|e| format!("utf8: {e}"))?;

    let (method_pass, server) = full.split_once('@').ok_or("invalid ss format")?;
    let (method, password) = method_pass
        .split_once(':')
        .ok_or("invalid method:password")?;
    let (host, port) = parse_host_port(server)?;

    Ok(ProxyNode::Shadowsocks {
        name: fragment,
        host,
        port,
        method: method.to_string(),
        password: password.to_string(),
        plugin: None,
        plugin_opts: None,
    })
}

fn parse_ss_plugin(query: Option<&str>) -> (Option<String>, Option<String>) {
    let q = match query {
        Some(q) => q,
        None => return (None, None),
    };
    let mut plugin = None;
    let mut plugin_opts = None;
    for pair in q.split('&') {
        if let Some((k, v)) = pair.split_once('=') {
            let v = urlencoding::decode(v)
                .unwrap_or_else(|_| v.into())
                .to_string();
            match k {
                "plugin" => plugin = Some(v),
                "plugin-opts" | "plugin_opts" => plugin_opts = Some(v),
                _ => {}
            }
        }
    }
    (plugin, plugin_opts)
}

fn parse_hysteria(input: &str) -> Result<ProxyNode, String> {
    let parsed = parse_url_based(input, "hysteria")?;

    let host = parsed.host_str().ok_or("missing host")?.to_string();
    let port = parsed.port().unwrap_or(443);

    let sni = get_query(&parsed, "peer").or_else(|| get_query(&parsed, "sni"));
    let auth = get_query(&parsed, "auth");
    let insecure = get_query(&parsed, "insecure").is_some_and(|v| v == "1" || v == "true");
    let up_mbps = get_query(&parsed, "upmbps")
        .and_then(|v| v.parse().ok())
        .unwrap_or(100);
    let down_mbps = get_query(&parsed, "downmbps")
        .and_then(|v| v.parse().ok())
        .unwrap_or(100);
    let obfs = get_query(&parsed, "obfsParam").or_else(|| get_query(&parsed, "obfs"));

    let name = parsed.fragment().map(|f| {
        urlencoding::decode(f)
            .unwrap_or_else(|_| f.into())
            .to_string()
    });

    Ok(ProxyNode::Hysteria {
        name,
        host,
        port,
        auth,
        sni,
        insecure,
        up_mbps,
        down_mbps,
        obfs,
    })
}

fn parse_tuic(input: &str) -> Result<ProxyNode, String> {
    let parsed = parse_url_based(input, "tuic")?;

    let uuid = parsed.username().to_string();
    if uuid.is_empty() {
        return Err("missing UUID in tuic URI".into());
    }

    let password = parsed.password().unwrap_or("").to_string();
    let host = parsed.host_str().ok_or("missing host")?.to_string();
    let port = parsed.port().unwrap_or(443);

    let sni = get_query(&parsed, "sni");
    let insecure = get_query(&parsed, "allowinsecure")
        .or_else(|| get_query(&parsed, "insecure"))
        .is_some_and(|v| v == "1" || v == "true");
    let congestion_control = get_query(&parsed, "congestion_control")
        .or_else(|| get_query(&parsed, "congestioncontrol"))
        .unwrap_or_else(|| "bbr".into());
    let udp_relay_mode =
        get_query(&parsed, "udp_relay_mode").or_else(|| get_query(&parsed, "udprelaymode"));
    let alpn =
        get_query(&parsed, "alpn").map(|a| a.split(',').map(|s| s.trim().to_string()).collect());

    let name = parsed.fragment().map(|f| {
        urlencoding::decode(f)
            .unwrap_or_else(|_| f.into())
            .to_string()
    });

    Ok(ProxyNode::Tuic {
        name,
        host,
        port,
        uuid,
        password,
        sni,
        insecure,
        congestion_control,
        udp_relay_mode,
        alpn,
    })
}

fn parse_wireguard(input: &str) -> Result<ProxyNode, String> {
    let scheme = if input.starts_with("wireguard://") {
        "wireguard"
    } else {
        "wg"
    };
    let parsed = parse_url_based(input, scheme)?;

    let private_key = urlencoding::decode(parsed.username())
        .unwrap_or_else(|_| parsed.username().into())
        .to_string();
    if private_key.is_empty() {
        return Err("missing private key in wireguard URI".into());
    }

    let host = parsed.host_str().ok_or("missing host")?.to_string();
    let port = parsed.port().unwrap_or(51820);

    let peer_public_key = get_query(&parsed, "publickey")
        .or_else(|| get_query(&parsed, "pub"))
        .or_else(|| get_query(&parsed, "peerpublickey"));
    let pre_shared_key = get_query(&parsed, "presharedkey").or_else(|| get_query(&parsed, "psk"));
    let mtu = get_query(&parsed, "mtu")
        .and_then(|v| v.parse().ok())
        .unwrap_or(1280);

    let local_address = get_query(&parsed, "address")
        .or_else(|| get_query(&parsed, "ip"))
        .or_else(|| get_query(&parsed, "localaddress"))
        .map(|a| a.split(',').map(|s| s.trim().to_string()).collect())
        .unwrap_or_default();

    let reserved = get_query(&parsed, "reserved").and_then(|v| {
        let parts: Result<Vec<u8>, _> = v.split(',').map(|s| s.trim().parse()).collect();
        parts.ok()
    });

    let name = parsed.fragment().map(|f| {
        urlencoding::decode(f)
            .unwrap_or_else(|_| f.into())
            .to_string()
    });

    Ok(ProxyNode::WireGuard {
        name,
        host,
        port,
        private_key,
        peer_public_key,
        pre_shared_key,
        local_address,
        reserved,
        mtu,
    })
}

fn parse_host_port(s: &str) -> Result<(String, u16), String> {
    // Handle [ipv6]:port
    if s.starts_with('[') {
        let end = s.find(']').ok_or("missing ] in IPv6 address")?;
        let host = s[1..end].to_string();
        let port_str = s.get(end + 2..).ok_or("missing port after IPv6")?;
        let port: u16 = port_str.parse().map_err(|_| "invalid port")?;
        Ok((host, port))
    } else {
        let (host, port_str) = s.rsplit_once(':').ok_or("missing port")?;
        let port: u16 = port_str.parse().map_err(|_| "invalid port")?;
        Ok((host.to_string(), port))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parse_hy2_full_uri() {
        let uri = "hysteria2://dongtaiwang.com@195.154.54.131:40022?sni=www.microsoft.com&insecure=1&mport=41000-42000#Hysteria2%E8%8A%82%E7%82%B9";
        let node = ProxyNode::parse(uri).unwrap();
        assert_eq!(node.name(), Some("Hysteria2节点"));
        assert_eq!(node.server_addr(), "195.154.54.131:41000-42000");
    }

    #[test]
    fn parse_vmess_uri() {
        let uri = "vmess://ew0KICAidiI6ICIyIiwNCiAgInBzIjogIlZNRVNT6IqC54K5Mi1pcHY2IiwNCiAgImFkZCI6ICIyMDAxOmJjODozMmQ3OjIyNTo6MSIsDQogICJwb3J0IjogIjIzNDU2IiwNCiAgImlkIjogImJkNDhlMGFjLWU5MGUtNDQ1MS1hNGYyLWFiZjA2MWNmYjg5MCIsDQogICJhaWQiOiAiMCIsDQogICJzY3kiOiAiYXV0byIsDQogICJuZXQiOiAid3MiLA0KICAidHlwZSI6ICJub25lIiwNCiAgImhvc3QiOiAid3d3LmJpbmcuY29tIiwNCiAgInBhdGgiOiAiL2FsdmluOTk5OS5jb20iLA0KICAidGxzIjogIiIsDQogICJzbmkiOiAiIiwNCiAgImFscG4iOiAiIiwNCiAgImZwIjogIiINCn0=";
        let node = ProxyNode::parse(uri).unwrap();
        assert_eq!(node.name(), Some("VMESS节点2-ipv6"));
    }

    #[test]
    fn parse_vless_reality() {
        let uri = "vless://28148be2-61a7-4f36-8adb-030cc9f82bd0@195.154.56.101:32202?encryption=none&security=reality&sni=www.amazon.com&fp=chrome&pbk=bGe_-nAtOq6_w_2mv6pcmD8RzJP65Tti-vyMP2hAwDc&sid=f57d8f26&type=xhttp&path=%2FG3EEoH63#VLESS-Reality";
        let node = ProxyNode::parse(uri).unwrap();
        assert_eq!(node.name(), Some("VLESS-Reality"));
        assert_eq!(node.protocol(), "vless");
        if let ProxyNode::Vless {
            security,
            pbk,
            sid,
            transport,
            ..
        } = &node
        {
            assert_eq!(security, "reality");
            assert!(pbk.is_some());
            assert_eq!(sid.as_deref(), Some("f57d8f26"));
            assert_eq!(transport, "xhttp");
        }
    }

    #[test]
    fn parse_trojan_uri() {
        let uri = "trojan://mypass@example.com:443?sni=example.com#MyTrojan";
        let node = ProxyNode::parse(uri).unwrap();
        assert_eq!(node.name(), Some("MyTrojan"));
        assert_eq!(node.protocol(), "trojan");
    }

    #[test]
    fn parse_ss_uri() {
        let uri = "ss://YWVzLTI1Ni1nY206dGVzdHBhc3M@1.2.3.4:8388#MySSNode";
        let node = ProxyNode::parse(uri).unwrap();
        assert_eq!(node.name(), Some("MySSNode"));
        assert_eq!(node.protocol(), "ss");
        if let ProxyNode::Shadowsocks {
            method, password, ..
        } = &node
        {
            assert_eq!(method, "aes-256-gcm");
            assert_eq!(password, "testpass");
        }
    }

    #[test]
    fn parse_hy2_obfs() {
        let uri = "hysteria2://pass@1.2.3.4:443?obfs=salamander&obfs-password=secret#ObfsNode";
        let node = ProxyNode::parse(uri).unwrap();
        if let ProxyNode::Hysteria2 {
            obfs,
            obfs_password,
            ..
        } = &node
        {
            assert_eq!(obfs.as_deref(), Some("salamander"));
            assert_eq!(obfs_password.as_deref(), Some("secret"));
        } else {
            panic!("expected Hysteria2");
        }
        let out = node.to_outbound("test");
        assert_eq!(out["obfs"]["type"], "salamander");
        assert_eq!(out["obfs"]["password"], "secret");
    }

    #[test]
    fn parse_vmess_fingerprint() {
        let uri = "vmess://ew0KICAidiI6ICIyIiwNCiAgInBzIjogImZwLW5vZGUiLA0KICAiYWRkIjogIjEuMi4zLjQiLA0KICAicG9ydCI6ICI0NDMiLA0KICAiaWQiOiAiYmQ0OGUwYWMtZTkwZS00NDUxLWE0ZjItYWJmMDYxY2ZiODkwIiwNCiAgImFpZCI6ICIwIiwNCiAgInNjeSI6ICJhdXRvIiwNCiAgIm5ldCI6ICJ3cyIsDQogICJ0eXBlIjogIm5vbmUiLA0KICAiaG9zdCI6ICIiLA0KICAicGF0aCI6ICIiLA0KICAidGxzIjogInRscyIsDQogICJzbmkiOiAiZXhhbXBsZS5jb20iLA0KICAiYWxwbiI6ICIiLA0KICAiZnAiOiAiY2hyb21lIg0KfQ==";
        let node = ProxyNode::parse(uri).unwrap();
        if let ProxyNode::Vmess {
            fingerprint, tls, ..
        } = &node
        {
            assert!(tls);
            assert_eq!(fingerprint.as_deref(), Some("chrome"));
        } else {
            panic!("expected Vmess");
        }
        let out = node.to_outbound("test");
        assert_eq!(out["tls"]["utls"]["fingerprint"], "chrome");
    }

    #[test]
    fn parse_trojan_reality() {
        let uri = "trojan://pass@1.2.3.4:443?security=reality&sni=www.google.com&fp=chrome&pbk=testkey&sid=ab12#TrojanReality";
        let node = ProxyNode::parse(uri).unwrap();
        if let ProxyNode::Trojan {
            security, pbk, sid, ..
        } = &node
        {
            assert_eq!(security, "reality");
            assert_eq!(pbk.as_deref(), Some("testkey"));
            assert_eq!(sid.as_deref(), Some("ab12"));
        } else {
            panic!("expected Trojan");
        }
        let out = node.to_outbound("test");
        assert_eq!(out["tls"]["reality"]["enabled"], true);
        assert_eq!(out["tls"]["reality"]["public_key"], "testkey");
    }

    #[test]
    fn parse_ss_plugin() {
        let uri = "ss://YWVzLTI1Ni1nY206dGVzdHBhc3M@1.2.3.4:8388?plugin=obfs-local&plugin-opts=obfs%3Dhttp%3Bobfs-host%3Dexample.com#SSPlugin";
        let node = ProxyNode::parse(uri).unwrap();
        if let ProxyNode::Shadowsocks {
            plugin,
            plugin_opts,
            ..
        } = &node
        {
            assert_eq!(plugin.as_deref(), Some("obfs-local"));
            assert!(plugin_opts.is_some());
        } else {
            panic!("expected Shadowsocks");
        }
        let out = node.to_outbound("test");
        assert_eq!(out["plugin"], "obfs-local");
    }

    #[test]
    fn parse_tuic_uri() {
        let uri = "tuic://uuid-1234:mypassword@1.2.3.4:443?congestion_control=bbr&udp_relay_mode=native&sni=example.com#MyTUIC";
        let node = ProxyNode::parse(uri).unwrap();
        assert_eq!(node.name(), Some("MyTUIC"));
        assert_eq!(node.protocol(), "tuic");
        if let ProxyNode::Tuic {
            uuid,
            password,
            congestion_control,
            udp_relay_mode,
            sni,
            ..
        } = &node
        {
            assert_eq!(uuid, "uuid-1234");
            assert_eq!(password, "mypassword");
            assert_eq!(congestion_control, "bbr");
            assert_eq!(udp_relay_mode.as_deref(), Some("native"));
            assert_eq!(sni.as_deref(), Some("example.com"));
        } else {
            panic!("expected Tuic");
        }
        let out = node.to_outbound("test");
        assert_eq!(out["type"], "tuic");
        assert_eq!(out["congestion_control"], "bbr");
    }

    #[test]
    fn parse_hysteria_v1_uri() {
        let uri = "hysteria://1.2.3.4:443?peer=example.com&auth=myauth&upmbps=50&downmbps=100&insecure=1&obfsParam=obfspass#HyV1";
        let node = ProxyNode::parse(uri).unwrap();
        assert_eq!(node.name(), Some("HyV1"));
        assert_eq!(node.protocol(), "hysteria");
        if let ProxyNode::Hysteria {
            auth,
            sni,
            up_mbps,
            down_mbps,
            obfs,
            insecure,
            ..
        } = &node
        {
            assert_eq!(auth.as_deref(), Some("myauth"));
            assert_eq!(sni.as_deref(), Some("example.com"));
            assert_eq!(*up_mbps, 50);
            assert_eq!(*down_mbps, 100);
            assert!(insecure);
            assert_eq!(obfs.as_deref(), Some("obfspass"));
        } else {
            panic!("expected Hysteria");
        }
        let out = node.to_outbound("test");
        assert_eq!(out["type"], "hysteria");
        assert_eq!(out["auth_str"], "myauth");
        assert_eq!(out["obfs"], "obfspass");
    }

    #[test]
    fn parse_wireguard_uri() {
        let uri = "wg://cHJpdmF0ZWtleQ%3D%3D@1.2.3.4:51820?publickey=pubkey123&address=10.0.0.1/32&reserved=1,2,3&mtu=1400#MyWG";
        let node = ProxyNode::parse(uri).unwrap();
        assert_eq!(node.name(), Some("MyWG"));
        assert_eq!(node.protocol(), "wireguard");
        if let ProxyNode::WireGuard {
            private_key,
            peer_public_key,
            local_address,
            reserved,
            mtu,
            ..
        } = &node
        {
            assert_eq!(private_key, "cHJpdmF0ZWtleQ==");
            assert_eq!(peer_public_key.as_deref(), Some("pubkey123"));
            assert_eq!(local_address, &vec!["10.0.0.1/32".to_string()]);
            assert_eq!(reserved.as_ref().unwrap(), &vec![1u8, 2, 3]);
            assert_eq!(*mtu, 1400);
        } else {
            panic!("expected WireGuard");
        }
        let out = node.to_outbound("test");
        assert_eq!(out["type"], "wireguard");
        assert_eq!(out["mtu"], 1400);
    }

    #[test]
    fn trojan_security_none_no_tls() {
        let uri = "trojan://pass@1.2.3.4:443?security=none#NoTLS";
        let node = ProxyNode::parse(uri).unwrap();
        let out = node.to_outbound("test");
        assert!(out.get("tls").is_none(), "security=none should not add TLS");
    }

    #[test]
    fn trojan_password_with_colon() {
        let uri = "trojan://user:pass@example.com:443#ColonPw";
        let node = ProxyNode::parse(uri).unwrap();
        if let ProxyNode::Trojan { password, .. } = &node {
            assert_eq!(password, "user:pass");
        } else {
            panic!("expected Trojan");
        }
    }

    #[test]
    fn hy2_password_with_colon() {
        let uri = "hysteria2://abc:def@1.2.3.4:443#HY2Colon";
        let node = ProxyNode::parse(uri).unwrap();
        if let ProxyNode::Hysteria2 { password, .. } = &node {
            assert_eq!(password, "abc:def");
        } else {
            panic!("expected Hysteria2");
        }
    }

    #[test]
    fn vmess_h2_transport() {
        use base64::Engine;
        let json = r#"{"v":"2","ps":"h2-node","add":"1.2.3.4","port":"443","id":"uuid-1234","aid":"0","scy":"auto","net":"h2","type":"none","host":"example.com","path":"/h2path","tls":"tls","sni":"example.com","fp":""}"#;
        let encoded = base64::engine::general_purpose::STANDARD.encode(json);
        let uri = format!("vmess://{encoded}");
        let node = ProxyNode::parse(&uri).unwrap();
        let out = node.to_outbound("test");
        assert_eq!(out["transport"]["type"], "http");
        assert_eq!(out["transport"]["path"], "/h2path");
    }

    #[test]
    fn vless_sni_fallback_to_host() {
        // No sni param — should use host as server_name
        let uri = "vless://uuid-1234@example.com:443?security=tls&type=tcp#NoSNI";
        let node = ProxyNode::parse(uri).unwrap();
        let out = node.to_outbound("test");
        assert_eq!(out["tls"]["server_name"], "example.com");
    }

    #[test]
    fn trojan_sni_fallback_to_host() {
        let uri = "trojan://pass@example.com:443#NoSNI";
        let node = ProxyNode::parse(uri).unwrap();
        let out = node.to_outbound("test");
        assert_eq!(out["tls"]["server_name"], "example.com");
    }

    #[test]
    fn vless_quic_transport() {
        let uri = "vless://uuid-1234@1.2.3.4:443?security=tls&type=quic#QUIC";
        let node = ProxyNode::parse(uri).unwrap();
        let out = node.to_outbound("test");
        assert_eq!(out["transport"]["type"], "quic");
    }

    #[test]
    fn hy2_multi_port_range() {
        let uri = "hysteria2://pass@1.2.3.4:443?mport=41000-42000,43000,44000-45000#MultiPort";
        let node = ProxyNode::parse(uri).unwrap();
        let out = node.to_outbound("test");
        let ports = out["server_ports"].as_array().unwrap();
        assert_eq!(ports.len(), 3);
        assert_eq!(ports[0], "41000:42000");
        assert_eq!(ports[1], "43000");
        assert_eq!(ports[2], "44000:45000");
    }

    #[test]
    fn vmess_ws_tls_has_alpn() {
        use base64::Engine;
        let json = r#"{"v":"2","ps":"ws-tls","add":"1.2.3.4","port":"443","id":"uuid-1234","aid":"0","scy":"auto","net":"ws","type":"none","host":"","path":"/ws","tls":"tls","sni":"example.com","fp":""}"#;
        let encoded = base64::engine::general_purpose::STANDARD.encode(json);
        let uri = format!("vmess://{encoded}");
        let node = ProxyNode::parse(&uri).unwrap();
        let out = node.to_outbound("test");
        let alpn = out["tls"]["alpn"].as_array().unwrap();
        assert!(alpn.contains(&serde_json::json!("h2")));
        assert!(alpn.contains(&serde_json::json!("http/1.1")));
    }

    #[test]
    fn parse_mole_server_uri() {
        // Exact format generated by scripts/mole-server.sh
        let uri = "vless://a1b2c3d4-e5f6-7890-abcd-ef1234567890@203.0.113.42:443?encryption=none&security=reality&sni=www.microsoft.com&fp=chrome&pbk=bGe_-nAtOq6_w_2mv6pcmD8RzJP65Tti-vyMP2hAwDc&sid=a1b2c3d4&type=tcp&flow=xtls-rprx-vision#mole-203.0.113.42";
        let node = ProxyNode::parse(uri).unwrap();
        assert_eq!(node.name(), Some("mole-203.0.113.42"));
        assert_eq!(node.protocol(), "vless");
        if let ProxyNode::Vless {
            uuid, host, port, security, sni, fingerprint, pbk, sid, transport, flow, ..
        } = &node
        {
            assert_eq!(uuid, "a1b2c3d4-e5f6-7890-abcd-ef1234567890");
            assert_eq!(host, "203.0.113.42");
            assert_eq!(*port, 443);
            assert_eq!(security, "reality");
            assert_eq!(sni.as_deref(), Some("www.microsoft.com"));
            assert_eq!(fingerprint.as_deref(), Some("chrome"));
            assert!(pbk.is_some());
            assert_eq!(sid.as_deref(), Some("a1b2c3d4"));
            assert_eq!(transport, "tcp");
            assert_eq!(flow.as_deref(), Some("xtls-rprx-vision"));
        } else {
            panic!("expected Vless");
        }
        // Verify outbound config generation
        let out = node.to_outbound("proxy");
        assert_eq!(out["type"], "vless");
        assert_eq!(out["server"], "203.0.113.42");
        assert_eq!(out["server_port"], 443);
        assert_eq!(out["tls"]["reality"]["enabled"], true);
        assert_eq!(out["tls"]["reality"]["public_key"], "bGe_-nAtOq6_w_2mv6pcmD8RzJP65Tti-vyMP2hAwDc");
    }
}
