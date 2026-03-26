use crate::uri::Hy2Uri;
use serde_json::{json, Value};

pub fn generate(uri: &Hy2Uri, bypass_cn: bool) -> Value {
    // --- DNS ---
    let mut dns_rules = vec![json!({ "clash_mode": "Direct", "server": "local" })];
    if bypass_cn {
        dns_rules.push(json!({ "rule_set": "geosite-cn", "server": "local" }));
    }

    let dns = json!({
        "servers": [
            {
                "tag": "remote",
                "type": "tls",
                "server": "8.8.8.8",
                "detour": "proxy"
            },
            {
                "tag": "local",
                "type": "udp",
                "server": "223.5.5.5"
            }
        ],
        "rules": dns_rules,
        "final": "remote",
        "strategy": "prefer_ipv4"
    });

    // --- Inbounds ---
    let inbounds = json!([{
        "type": "tun",
        "tag": "tun-in",
        "address": ["172.19.0.1/30"],
        "mtu": 1500,
        "auto_route": true,
        "strict_route": true,
        "stack": "system"
    }]);

    // --- Outbounds ---
    let mut tls = json!({ "enabled": true });
    if let Some(ref sni) = uri.sni {
        tls["server_name"] = json!(sni);
    }
    if uri.insecure {
        tls["insecure"] = json!(true);
    }

    let mut proxy = json!({
        "type": "hysteria2",
        "tag": "proxy",
        "server": uri.host,
        "server_port": uri.port,
        "password": uri.auth,
        "tls": tls
    });

    if let Some(ref hop) = uri.hop_ports {
        // sing-box uses colon notation for port ranges: "41000:42000"
        let port_range = hop.replace('-', ":");
        proxy["server_ports"] = json!([port_range]);
        proxy["hop_interval"] = json!("30s");
    }

    let outbounds = json!([
        proxy,
        { "type": "direct", "tag": "direct" }
    ]);

    // --- Route ---
    // Use rule actions (sing-box 1.11+) instead of legacy special outbounds
    let mut route_rules = vec![
        json!({ "action": "sniff" }),
        json!({ "protocol": "dns", "action": "hijack-dns" }),
        json!({ "ip_is_private": true, "outbound": "direct" }),
    ];

    if bypass_cn {
        route_rules.push(json!({
            "rule_set": ["geoip-cn", "geosite-cn"],
            "outbound": "direct"
        }));
    }

    let mut route = json!({
        "rules": route_rules,
        "auto_detect_interface": true,
        "default_domain_resolver": "local",
        "final": "proxy"
    });

    if bypass_cn {
        route["rule_set"] = json!([
            {
                "tag": "geoip-cn",
                "type": "remote",
                "format": "binary",
                "url": "https://raw.githubusercontent.com/SagerNet/sing-geoip/rule-set/geoip-cn.srs",
                "download_detour": "proxy"
            },
            {
                "tag": "geosite-cn",
                "type": "remote",
                "format": "binary",
                "url": "https://raw.githubusercontent.com/SagerNet/sing-geosite/rule-set/geosite-cn.srs",
                "download_detour": "proxy"
            }
        ]);
    }

    json!({
        "log": { "level": "info" },
        "dns": dns,
        "inbounds": inbounds,
        "outbounds": outbounds,
        "route": route
    })
}

pub fn to_json_pretty(config: &Value) -> String {
    serde_json::to_string_pretty(config).expect("serialize config")
}
