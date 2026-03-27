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
    },
    Vless {
        name: Option<String>,
        host: String,
        port: u16,
        uuid: String,
        flow: Option<String>,       // xtls-rprx-vision
        security: String,           // tls, reality, none
        sni: Option<String>,
        fingerprint: Option<String>,
        // Reality fields
        pbk: Option<String>,        // public key
        sid: Option<String>,        // short id
        // Transport
        transport: String,          // tcp, ws, grpc, xhttp
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
    },
}

impl ProxyNode {
    pub fn parse(input: &str) -> Result<Self, String> {
        if input.starts_with("hysteria2://") || input.starts_with("hy2://") {
            parse_hy2(input)
        } else if input.starts_with("vmess://") {
            parse_vmess(input)
        } else if input.starts_with("vless://") {
            parse_vless(input)
        } else if input.starts_with("trojan://") {
            parse_trojan(input)
        } else if input.starts_with("ss://") {
            parse_ss(input)
        } else {
            Err(format!("unsupported protocol: {}", input.split("://").next().unwrap_or("?")))
        }
    }

    pub fn name(&self) -> Option<&str> {
        match self {
            ProxyNode::Hysteria2 { name, .. }
            | ProxyNode::Vmess { name, .. }
            | ProxyNode::Vless { name, .. }
            | ProxyNode::Trojan { name, .. }
            | ProxyNode::Shadowsocks { name, .. } => name.as_deref(),
        }
    }

    pub fn protocol(&self) -> &str {
        match self {
            ProxyNode::Hysteria2 { .. } => "hysteria2",
            ProxyNode::Vmess { .. } => "vmess",
            ProxyNode::Vless { .. } => "vless",
            ProxyNode::Trojan { .. } => "trojan",
            ProxyNode::Shadowsocks { .. } => "ss",
        }
    }

    pub fn server_addr(&self) -> String {
        match self {
            ProxyNode::Hysteria2 { host, port, hop_ports, .. } => {
                if let Some(ref ports) = hop_ports {
                    format!("{host}:{ports}")
                } else {
                    format!("{host}:{port}")
                }
            }
            ProxyNode::Vmess { host, port, .. }
            | ProxyNode::Vless { host, port, .. }
            | ProxyNode::Trojan { host, port, .. }
            | ProxyNode::Shadowsocks { host, port, .. } => format!("{host}:{port}"),
        }
    }

    /// Generate sing-box outbound JSON
    pub fn to_outbound(&self) -> Value {
        match self {
            ProxyNode::Hysteria2 {
                host, port, password, sni, insecure, hop_ports, ..
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
                    "tag": "proxy",
                    "server": host,
                    "server_port": port,
                    "password": password,
                    "tls": tls
                });

                if let Some(ref hop) = hop_ports {
                    let port_range = hop.replace('-', ":");
                    out["server_ports"] = serde_json::json!([port_range]);
                    out["hop_interval"] = serde_json::json!("30s");
                }

                out["up_mbps"] = serde_json::json!(100);
                out["down_mbps"] = serde_json::json!(200);

                out
            }

            ProxyNode::Vmess {
                host, port, uuid, alter_id, security,
                transport, ws_host, ws_path, tls, sni, ..
            } => {
                let mut out = serde_json::json!({
                    "type": "vmess",
                    "tag": "proxy",
                    "server": host,
                    "server_port": port,
                    "uuid": uuid,
                    "alter_id": alter_id,
                    "security": security
                });

                build_transport(&mut out, transport, ws_path, ws_host, &None);

                if *tls {
                    let mut tls_obj = serde_json::json!({ "enabled": true });
                    if let Some(ref s) = sni {
                        tls_obj["server_name"] = serde_json::json!(s);
                    }
                    out["tls"] = tls_obj;
                }

                out
            }

            ProxyNode::Vless {
                host, port, uuid, flow, security, sni, fingerprint,
                pbk, sid, transport, ws_path, ws_host, grpc_service, ..
            } => {
                let mut out = serde_json::json!({
                    "type": "vless",
                    "tag": "proxy",
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
                if security == "reality" {
                    let mut tls = serde_json::json!({
                        "enabled": true,
                        "reality": { "enabled": true }
                    });
                    if let Some(ref s) = sni {
                        tls["server_name"] = serde_json::json!(s);
                    }
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
                    let mut tls = serde_json::json!({ "enabled": true });
                    if let Some(ref s) = sni {
                        tls["server_name"] = serde_json::json!(s);
                    }
                    if let Some(ref fp) = fingerprint {
                        tls["utls"] = serde_json::json!({ "enabled": true, "fingerprint": fp });
                    }
                    out["tls"] = tls;
                }

                build_transport(&mut out, transport, ws_path, ws_host, grpc_service);

                out
            }

            ProxyNode::Trojan {
                host, port, password, sni, fingerprint,
                transport, ws_path, ws_host, ..
            } => {
                let mut tls = serde_json::json!({ "enabled": true });
                if let Some(ref s) = sni {
                    tls["server_name"] = serde_json::json!(s);
                }
                if let Some(ref fp) = fingerprint {
                    tls["utls"] = serde_json::json!({ "enabled": true, "fingerprint": fp });
                }

                let mut out = serde_json::json!({
                    "type": "trojan",
                    "tag": "proxy",
                    "server": host,
                    "server_port": port,
                    "password": password,
                    "tls": tls
                });

                build_transport(&mut out, transport, ws_path, ws_host, &None);

                out
            }

            ProxyNode::Shadowsocks {
                host, port, method, password, ..
            } => {
                serde_json::json!({
                    "type": "shadowsocks",
                    "tag": "proxy",
                    "server": host,
                    "server_port": port,
                    "method": method,
                    "password": password
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
        _ => {} // tcp = no transport needed
    }
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

    let auth = parsed.username().to_string();
    if auth.is_empty() {
        return Err("missing auth (password) in URI".into());
    }

    let host = parsed.host_str().ok_or("missing host")?.to_string();
    let port = parsed.port().unwrap_or(443);

    let mut sni = None;
    let mut insecure = false;
    let mut hop_ports = None;

    for (k, v) in parsed.query_pairs() {
        match k.as_ref() {
            "sni" => sni = Some(v.to_string()),
            "insecure" => insecure = v == "1" || v == "true",
            "mport" => hop_ports = Some(v.to_string()),
            _ => {}
        }
    }

    let name = parsed.fragment().map(|f| {
        urlencoding::decode(f).unwrap_or_else(|_| f.into()).to_string()
    });

    Ok(ProxyNode::Hysteria2 { name, host, port, password: auth, sni, insecure, hop_ports })
}

fn parse_vmess(input: &str) -> Result<ProxyNode, String> {
    let b64 = input.strip_prefix("vmess://").ok_or("missing vmess:// prefix")?;

    use base64::Engine;
    let decoded = base64::engine::general_purpose::STANDARD
        .decode(b64)
        .or_else(|_| base64::engine::general_purpose::STANDARD_NO_PAD.decode(b64))
        .map_err(|e| format!("base64 decode: {e}"))?;

    let json: Value = serde_json::from_slice(&decoded)
        .map_err(|e| format!("JSON parse: {e}"))?;

    let host = json["add"].as_str().ok_or("missing 'add' field")?.to_string();
    let port: u16 = json["port"].as_str().and_then(|s| s.parse().ok())
        .or_else(|| json["port"].as_u64().map(|n| n as u16))
        .ok_or("missing/invalid 'port'")?;
    let uuid = json["id"].as_str().ok_or("missing 'id' field")?.to_string();
    let alter_id: u16 = json["aid"].as_str().and_then(|s| s.parse().ok())
        .or_else(|| json["aid"].as_u64().map(|n| n as u16))
        .unwrap_or(0);
    let security = json["scy"].as_str().unwrap_or("auto").to_string();
    let transport = json["net"].as_str().unwrap_or("tcp").to_string();
    let name = json["ps"].as_str().map(|s| s.to_string());
    let ws_host = json["host"].as_str().filter(|s| !s.is_empty()).map(|s| s.to_string());
    let ws_path = json["path"].as_str().filter(|s| !s.is_empty()).map(|s| s.to_string());
    let tls = json["tls"].as_str().unwrap_or("") == "tls";
    let sni = json["sni"].as_str().filter(|s| !s.is_empty()).map(|s| s.to_string());

    Ok(ProxyNode::Vmess { name, host, port, uuid, alter_id, security, transport, ws_host, ws_path, tls, sni })
}

fn parse_url_based(input: &str, scheme: &str) -> Result<Url, String> {
    let fake = input.replacen(&format!("{scheme}://"), "https://", 1);
    Url::parse(&fake).map_err(|e| format!("invalid URI: {e}"))
}

fn get_query(parsed: &Url, key: &str) -> Option<String> {
    parsed.query_pairs()
        .find(|(k, _)| k == key)
        .map(|(_, v)| v.to_string())
        .filter(|v| !v.is_empty())
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
        urlencoding::decode(f).unwrap_or_else(|_| f.into()).to_string()
    });

    Ok(ProxyNode::Vless {
        name, host, port, uuid, flow, security, sni, fingerprint,
        pbk, sid, transport, ws_path, ws_host, grpc_service,
    })
}

fn parse_trojan(input: &str) -> Result<ProxyNode, String> {
    let parsed = parse_url_based(input, "trojan")?;

    let password = parsed.username().to_string();
    if password.is_empty() {
        return Err("missing password in trojan URI".into());
    }

    let host = parsed.host_str().ok_or("missing host")?.to_string();
    let port = parsed.port().unwrap_or(443);

    let sni = get_query(&parsed, "sni");
    let fingerprint = get_query(&parsed, "fp");
    let transport = get_query(&parsed, "type").unwrap_or_else(|| "tcp".into());
    let ws_path = get_query(&parsed, "path");
    let ws_host = get_query(&parsed, "host");

    let name = parsed.fragment().map(|f| {
        urlencoding::decode(f).unwrap_or_else(|_| f.into()).to_string()
    });

    Ok(ProxyNode::Trojan { name, host, port, password, sni, fingerprint, transport, ws_path, ws_host })
}

fn parse_ss(input: &str) -> Result<ProxyNode, String> {
    use base64::Engine;

    let rest = input.strip_prefix("ss://").ok_or("missing ss:// prefix")?;

    // Format: ss://base64(method:password)@host:port#name
    // Or:     ss://base64(method:password@host:port)#name
    let (encoded, fragment) = match rest.split_once('#') {
        Some((e, f)) => (e, Some(urlencoding::decode(f).unwrap_or_else(|_| f.into()).to_string())),
        None => (rest, None),
    };

    // Try format: base64@host:port
    if let Some((b64, server)) = encoded.split_once('@') {
        let decoded = base64::engine::general_purpose::STANDARD.decode(b64)
            .or_else(|_| base64::engine::general_purpose::STANDARD_NO_PAD.decode(b64))
            .map_err(|e| format!("base64 decode: {e}"))?;
        let cred = String::from_utf8(decoded).map_err(|e| format!("utf8: {e}"))?;

        let (method, password) = cred.split_once(':').ok_or("invalid method:password")?;

        // Parse host:port - handle IPv6 [addr]:port
        let (host, port) = parse_host_port(server)?;

        return Ok(ProxyNode::Shadowsocks {
            name: fragment, host, port, method: method.to_string(), password: password.to_string(),
        });
    }

    // Try format: base64(method:password@host:port)
    let decoded = base64::engine::general_purpose::STANDARD.decode(encoded)
        .or_else(|_| base64::engine::general_purpose::STANDARD_NO_PAD.decode(encoded))
        .map_err(|e| format!("base64 decode: {e}"))?;
    let full = String::from_utf8(decoded).map_err(|e| format!("utf8: {e}"))?;

    let (method_pass, server) = full.split_once('@').ok_or("invalid ss format")?;
    let (method, password) = method_pass.split_once(':').ok_or("invalid method:password")?;
    let (host, port) = parse_host_port(server)?;

    Ok(ProxyNode::Shadowsocks {
        name: fragment, host, port, method: method.to_string(), password: password.to_string(),
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
        if let ProxyNode::Vless { security, pbk, sid, transport, .. } = &node {
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
        if let ProxyNode::Shadowsocks { method, password, .. } = &node {
            assert_eq!(method, "aes-256-gcm");
            assert_eq!(password, "testpass");
        }
    }
}
