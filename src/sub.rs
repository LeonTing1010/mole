use crate::uri::ProxyNode;
use std::time::Duration;

/// Fetch subscription content from a URL.
pub fn fetch(url: &str) -> Result<String, String> {
    let resp = reqwest::blocking::Client::builder()
        .timeout(Duration::from_secs(30))
        .build()
        .map_err(|e| format!("http client: {e}"))?
        .get(url)
        .header("User-Agent", "Mole/0.3")
        .send()
        .map_err(|e| format!("fetch failed: {e}"))?;

    if !resp.status().is_success() {
        return Err(format!("HTTP {}", resp.status()));
    }

    resp.text().map_err(|e| format!("read body: {e}"))
}

/// Parse subscription body into a list of proxy URIs.
/// Supports base64-encoded and plain-text (one URI per line) formats.
pub fn parse(body: &str) -> Vec<String> {
    use base64::Engine;
    let trimmed = body.trim();

    // Try base64 decode first
    if let Ok(decoded) = base64::engine::general_purpose::STANDARD
        .decode(trimmed)
        .or_else(|_| base64::engine::general_purpose::STANDARD_NO_PAD.decode(trimmed))
    {
        if let Ok(text) = String::from_utf8(decoded) {
            let uris: Vec<String> = text
                .lines()
                .map(|l| l.trim().to_string())
                .filter(|l| !l.is_empty() && l.contains("://"))
                .collect();
            if !uris.is_empty() {
                return uris;
            }
        }
    }

    // Fall back to plain text
    body.lines()
        .map(|l| l.trim().to_string())
        .filter(|l| !l.is_empty() && l.contains("://"))
        .collect()
}

pub fn node_display_name(node: &ProxyNode) -> String {
    let proto = match node.protocol() {
        "hysteria2" => "hy2",
        "hysteria" => "hy1",
        "vmess" => "vmess",
        "vless" => "vless",
        "trojan" => "trojan",
        "ss" => "ss",
        "tuic" => "tuic",
        "wireguard" => "wg",
        other => other,
    };
    let addr = node.server_addr();
    format!("{proto}-{addr}")
}

pub fn parse_nodes(body: &str) -> Vec<(String, String)> {
    parse(body)
        .into_iter()
        .filter_map(|uri| {
            let node = ProxyNode::parse(&uri).ok()?;
            let name = node
                .name()
                .map(|s| s.to_string())
                .unwrap_or_else(|| node_display_name(&node));
            Some((name, uri))
        })
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parse_base64_subscription() {
        use base64::Engine;
        let uris = "trojan://pass@example.com:443#Node1\nss://YWVzLTI1Ni1nY206dGVzdHBhc3M@1.2.3.4:8388#Node2\n";
        let encoded = base64::engine::general_purpose::STANDARD.encode(uris);
        let result = parse(&encoded);
        assert_eq!(result.len(), 2);
        assert!(result[0].starts_with("trojan://"));
        assert!(result[1].starts_with("ss://"));
    }

    #[test]
    fn parse_plain_text_subscription() {
        let body = "trojan://pass@example.com:443#Node1\nss://YWVzLTI1Ni1nY206dGVzdHBhc3M@1.2.3.4:8388#Node2\n";
        let result = parse(body);
        assert_eq!(result.len(), 2);
    }

    #[test]
    fn parse_skips_invalid_lines() {
        let body = "trojan://pass@example.com:443#Good\nthis is not a uri\n\n# comment\nhysteria2://pw@1.2.3.4:443#Also-Good\n";
        let result = parse(body);
        assert_eq!(result.len(), 2);
    }

    #[test]
    fn parse_nodes_skips_unparseable() {
        let body = "trojan://pass@example.com:443#Good\nunknown://bad\n";
        let result = parse_nodes(body);
        assert_eq!(result.len(), 1);
        assert_eq!(result[0].0, "Good");
    }
}
