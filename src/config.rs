use crate::store::Rule;
use crate::uri::ProxyNode;
use serde_json::{json, Value};

/// Default LAN proxy port for --share mode.
pub const SHARE_PORT: u16 = 7890;

/// Generate a complete sing-box configuration.
///
/// - `nodes`: `(tag, node)` pairs. Single-node mode: one entry tagged `"proxy"`.
///   Strategy mode: multiple entries with unique tags.
/// - `custom_rules`: user-defined routing rules from the Store.
/// - `strategy`: `None` = single node, `Some("urltest"|"fallback"|"select")` = wrap all nodes.
/// - `share`: if true, add a mixed HTTP/SOCKS inbound on LAN.
pub fn generate(
    nodes: &[(&str, &ProxyNode)],
    custom_rules: &[Rule],
    bypass_cn: bool,
    strategy: Option<&str>,
    share: bool,
) -> Value {
    let mole_dir = dirs::home_dir().expect("home dir").join(".mole");

    // --- DNS ---
    // Domains routed to "direct" must also resolve via local DNS (223.5.5.5),
    // otherwise DNS goes through remote/proxy causing 5-10s delays.
    let mut dns_rules = vec![json!({ "clash_mode": "Direct", "server": "local" })];
    for rule in custom_rules {
        if rule.outbound == "direct" {
            let dns_rule = match rule.match_type.as_str() {
                "domain" => json!({ "domain": [&rule.pattern], "server": "local" }),
                "domain_suffix" => json!({ "domain_suffix": [&rule.pattern], "server": "local" }),
                "domain_keyword" => json!({ "domain_keyword": [&rule.pattern], "server": "local" }),
                _ => continue,
            };
            dns_rules.push(dns_rule);
        }
    }
    if bypass_cn {
        dns_rules.push(json!({ "rule_set": "geosite-cn", "server": "local" }));
    }

    // Smart routing: resolve ALL domains via local DNS (fast, ~20ms).
    // Chinese domains get Chinese IPs → geoip-cn matches → direct.
    // Foreign domains get some IP → no geoip match → proxy.
    // Proxy outbound re-resolves using sniffed domain, so local DNS
    // result doesn't affect proxy connection quality.
    let dns_final = if bypass_cn { "local" } else { "remote" };

    let dns = json!({
        "servers": [
            { "tag": "remote", "type": "tls", "server": "8.8.8.8", "detour": "proxy" },
            { "tag": "local", "type": "udp", "server": "223.5.5.5" }
        ],
        "rules": dns_rules,
        "final": dns_final,
        "strategy": "prefer_ipv4"
    });

    // --- Inbounds ---
    let mut inbound_list = vec![json!({
        "type": "tun", "tag": "tun-in",
        "address": ["172.19.0.1/30", "fdfe:dcba:9876::1/126"],
        "mtu": 1500, "auto_route": true, "strict_route": true, "stack": "mixed"
    })];

    if share {
        inbound_list.push(json!({
            "type": "mixed",
            "tag": "lan-in",
            "listen": "::",
            "listen_port": SHARE_PORT
        }));
    }

    let inbounds = serde_json::Value::Array(inbound_list);

    // --- Outbounds ---
    let mut outbounds = Vec::new();
    let needs_block = custom_rules.iter().any(|r| r.outbound == "block");

    match strategy {
        Some(strat_type) if nodes.len() > 1 => {
            // Strategy mode: wrap all nodes
            let node_tags: Vec<Value> = nodes.iter().map(|(tag, _)| json!(tag)).collect();

            let mut strat = json!({
                "type": strat_type,
                "tag": "proxy",
                "outbounds": node_tags,
            });
            if strat_type == "urltest" {
                strat["interval"] = json!("5m");
                strat["url"] = json!("https://www.gstatic.com/generate_204");
            }
            outbounds.push(strat);

            for (tag, node) in nodes {
                let mut ob = node.to_outbound();
                ob["tag"] = json!(tag);
                outbounds.push(ob);
            }
        }
        _ => {
            // Single node mode
            if let Some((_, node)) = nodes.first() {
                outbounds.push(node.to_outbound());
            }
        }
    }

    outbounds.push(json!({ "type": "direct", "tag": "direct" }));
    if needs_block {
        outbounds.push(json!({ "type": "block", "tag": "block" }));
    }

    // --- Route rules ---
    let mut route_rules = vec![
        json!({ "action": "sniff" }),
        json!({ "protocol": "dns", "action": "hijack-dns" }),
        json!({ "ip_is_private": true, "outbound": "direct" }),
    ];

    // Custom rules
    for rule in custom_rules {
        let r = match rule.match_type.as_str() {
            "domain" => json!({ "domain": [&rule.pattern], "outbound": &rule.outbound }),
            "domain_suffix" => {
                json!({ "domain_suffix": [&rule.pattern], "outbound": &rule.outbound })
            }
            "domain_keyword" => {
                json!({ "domain_keyword": [&rule.pattern], "outbound": &rule.outbound })
            }
            "ip_cidr" => json!({ "ip_cidr": [&rule.pattern], "outbound": &rule.outbound }),
            "geoip" => json!({ "rule_set": format!("geoip-{}", rule.pattern), "outbound": &rule.outbound }),
            "geosite" => json!({ "rule_set": format!("geosite-{}", rule.pattern), "outbound": &rule.outbound }),
            _ => continue,
        };
        route_rules.push(r);
    }

    if bypass_cn {
        route_rules.push(json!({
            "rule_set": ["geoip-cn", "geosite-cn"],
            "outbound": "direct"
        }));
        // Force all IPv6 through proxy to prevent IPv6 leak
        route_rules.push(json!({
            "ip_cidr": ["::/0"],
            "outbound": "proxy"
        }));
    }

    let mut route = json!({
        "rules": route_rules,
        "auto_detect_interface": true,
        "default_domain_resolver": "local",
        "final": "proxy"
    });

    // --- Rule sets (geo data files) ---
    let mut rule_sets: Vec<Value> = Vec::new();

    if bypass_cn {
        rule_sets.push(json!({
            "tag": "geoip-cn", "type": "local", "format": "binary",
            "path": mole_dir.join("geoip-cn.srs").to_str().unwrap()
        }));
        rule_sets.push(json!({
            "tag": "geosite-cn", "type": "local", "format": "binary",
            "path": mole_dir.join("geosite-cn.srs").to_str().unwrap()
        }));
    }

    for rule in custom_rules {
        let tag = match rule.match_type.as_str() {
            "geoip" => format!("geoip-{}", rule.pattern),
            "geosite" => format!("geosite-{}", rule.pattern),
            _ => continue,
        };
        if !rule_sets.iter().any(|r| r["tag"].as_str() == Some(&tag)) {
            rule_sets.push(json!({
                "tag": &tag, "type": "local", "format": "binary",
                "path": mole_dir.join(format!("{tag}.srs")).to_str().unwrap()
            }));
        }
    }

    if !rule_sets.is_empty() {
        route["rule_set"] = json!(rule_sets);
    }

    json!({
        "log": { "level": "info" },
        "dns": dns,
        "inbounds": inbounds,
        "outbounds": outbounds,
        "route": route,
        "experimental": {
            "clash_api": { "external_controller": "127.0.0.1:19090" }
        }
    })
}

pub fn to_json_pretty(config: &Value) -> String {
    serde_json::to_string_pretty(config).expect("serialize config")
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::store::Rule;

    fn dummy_trojan() -> ProxyNode {
        ProxyNode::parse("trojan://pass@example.com:443#Test").unwrap()
    }

    fn dummy_ss() -> ProxyNode {
        ProxyNode::parse("ss://YWVzLTI1Ni1nY206dGVzdHBhc3M@1.2.3.4:8388#SS").unwrap()
    }

    #[test]
    fn single_node_has_proxy_tag() {
        let node = dummy_trojan();
        let cfg = generate(&[("proxy", &node)], &[], false, None, false);
        let outbounds = cfg["outbounds"].as_array().unwrap();
        assert_eq!(outbounds[0]["tag"], "proxy");
        assert_eq!(outbounds[1]["tag"], "direct");
    }

    #[test]
    fn strategy_wraps_nodes() {
        let n1 = dummy_trojan();
        let n2 = dummy_ss();
        let cfg = generate(&[("node-1", &n1), ("node-2", &n2)], &[], false, Some("urltest"), false);
        let outbounds = cfg["outbounds"].as_array().unwrap();
        // First outbound is the strategy wrapper
        assert_eq!(outbounds[0]["type"], "urltest");
        assert_eq!(outbounds[0]["tag"], "proxy");
        let inner = outbounds[0]["outbounds"].as_array().unwrap();
        assert_eq!(inner.len(), 2);
        // Individual nodes follow
        assert_eq!(outbounds[1]["tag"], "node-1");
        assert_eq!(outbounds[2]["tag"], "node-2");
    }

    #[test]
    fn custom_rules_injected() {
        let node = dummy_trojan();
        let rules = vec![
            Rule { match_type: "domain".into(), pattern: "example.com".into(), outbound: "direct".into() },
            Rule { match_type: "domain_suffix".into(), pattern: ".cn".into(), outbound: "direct".into() },
        ];
        let cfg = generate(&[("proxy", &node)], &rules, false, None, false);
        let route_rules = cfg["route"]["rules"].as_array().unwrap();
        // Should have: sniff, dns-hijack, ip_is_private, domain, domain_suffix
        assert!(route_rules.iter().any(|r| r.get("domain").is_some()));
        assert!(route_rules.iter().any(|r| r.get("domain_suffix").is_some()));
    }

    #[test]
    fn block_rule_adds_block_outbound() {
        let node = dummy_trojan();
        let rules = vec![
            Rule { match_type: "domain".into(), pattern: "ads.example.com".into(), outbound: "block".into() },
        ];
        let cfg = generate(&[("proxy", &node)], &rules, false, None, false);
        let outbounds = cfg["outbounds"].as_array().unwrap();
        assert!(outbounds.iter().any(|o| o["tag"] == "block"));
    }

    #[test]
    fn no_block_outbound_when_not_needed() {
        let node = dummy_trojan();
        let cfg = generate(&[("proxy", &node)], &[], false, None, false);
        let outbounds = cfg["outbounds"].as_array().unwrap();
        assert!(!outbounds.iter().any(|o| o["tag"] == "block"));
    }
}
