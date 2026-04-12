use crate::uri::ProxyNode;
use chrono::Local;
use std::time::Duration;

/// Fetch subscription content from a URL.
pub fn fetch(url: &str) -> Result<String, String> {
    let resp = reqwest::blocking::Client::builder()
        .timeout(Duration::from_secs(30))
        .build()
        .map_err(|e| format!("http client: {e}"))?
        .get(url)
        .header("User-Agent", "clash-verge/v2.2.3")
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

/// Extract proxy URIs from an HTML page (e.g. GitHub wiki).
/// Matches vless://, vmess://, trojan://, ss://, hysteria2:// etc. in the raw HTML.
/// Decodes HTML entities (&amp; etc.) and deduplicates by server address.
pub fn extract_uris_from_html(html: &str) -> Vec<String> {
    let mut uris = Vec::new();
    let mut start = 0;
    let protocols = ["vless://", "vmess://", "trojan://", "ss://", "hysteria2://", "hysteria://", "hy2://", "tuic://", "wireguard://"];
    while start < html.len() {
        let mut earliest: Option<(usize, &str)> = None;
        for proto in &protocols {
            if let Some(pos) = html[start..].find(proto) {
                let abs = start + pos;
                if earliest.is_none() || abs < earliest.unwrap().0 {
                    earliest = Some((abs, proto));
                }
            }
        }
        let Some((pos, _)) = earliest else { break };
        // Extract until whitespace, quote, or angle bracket
        let end = html[pos..]
            .find(|c: char| c.is_whitespace() || c == '"' || c == '\'' || c == '<' || c == '>')
            .map(|e| pos + e)
            .unwrap_or(html.len());
        let raw = &html[pos..end];
        if raw.len() > 10 {
            // Decode HTML entities
            let uri = raw.replace("&amp;", "&").replace("&lt;", "<").replace("&gt;", ">").replace("&#x2F;", "/");
            uris.push(uri);
        }
        start = end;
    }

    // Deduplicate: keep the longest URI per protocol+host+port
    // (short truncated URIs are subsets of the full one)
    uris.sort_by(|a, b| b.len().cmp(&a.len())); // longest first
    let mut seen_servers = std::collections::HashSet::new();
    let mut unique = Vec::new();
    for uri in &uris {
        // Extract server key: proto + host + port
        let key = if let Ok(node) = crate::uri::ProxyNode::parse(uri) {
            format!("{}:{}", node.protocol(), node.server_addr())
        } else {
            uri.clone() // unparseable — keep as-is
        };
        if seen_servers.insert(key) {
            unique.push(uri.clone());
        }
    }
    unique
}

/// Expand date-pattern URL and fetch nodes from all files.
/// Supports placeholders: {YYYY}, {MM}, {DD}, {YYYYMMDD}, {N}
/// Tries today first, falls back to yesterday if today returns no nodes.
pub fn fetch_date_pattern(pattern: &str, count: usize) -> Vec<(String, String)> {
    let count = if count == 0 { 1 } else { count };

    for days_ago in 0..=3 {
        let date = Local::now().date_naive() - chrono::Duration::days(days_ago);
        let yyyy = date.format("%Y").to_string();
        let mm = date.format("%m").to_string();
        let dd = date.format("%d").to_string();
        let yyyymmdd = date.format("%Y%m%d").to_string();

        let mut all_nodes = Vec::new();
        for n in 0..count {
            let url = pattern
                .replace("{YYYY}", &yyyy)
                .replace("{MM}", &mm)
                .replace("{DD}", &dd)
                .replace("{YYYYMMDD}", &yyyymmdd)
                .replace("{N}", &n.to_string());

            if let Ok(body) = fetch(&url) {
                let nodes = parse_nodes(&body);
                all_nodes.extend(nodes);
            }
        }
        if !all_nodes.is_empty() {
            return all_nodes;
        }
    }
    vec![]
}

pub fn parse_nodes(body: &str) -> Vec<(String, String)> {
    parse(body)
        .into_iter()
        .filter_map(|uri| {
            let node = ProxyNode::parse(&uri).ok()?;
            let name = node_display_name(&node);
            Some((name, uri))
        })
        .collect()
}

/// Parse proxy nodes from an HTML page (wiki, blog etc.)
/// If no direct proxy URIs found, looks for linked .txt subscription files and fetches those.
pub fn parse_nodes_from_html(html: &str) -> Vec<(String, String)> {
    // First try: direct proxy URIs in the HTML
    let direct = extract_uris_from_html(html);
    if !direct.is_empty() {
        return direct
            .into_iter()
            .filter_map(|uri| {
                let node = ProxyNode::parse(&uri).ok()?;
                let name = node_display_name(&node);
                Some((name, uri))
            })
            .collect();
    }

    // Second try: find linked pages that might contain nodes
    // Look for article links (e.g. *-node-share.htm) and .txt subscription files
    let mut all_nodes = Vec::new();

    // Find .txt subscription links
    let txt_urls: Vec<String> = extract_uris_from_html_by_ext(html, ".txt");
    for url in &txt_urls {
        if let Ok(body) = fetch(url) {
            all_nodes.extend(parse_nodes(&body));
        }
    }
    if !all_nodes.is_empty() {
        return all_nodes;
    }

    // Find article page links and recurse one level
    let article_links: Vec<String> = extract_uris_from_html_by_ext(html, "node-share");
    if let Some(link) = article_links.first() {
        if let Ok(article_html) = fetch(link) {
            let sub_urls = extract_uris_from_html_by_ext(&article_html, ".txt");
            for url in &sub_urls {
                if let Ok(body) = fetch(url) {
                    all_nodes.extend(parse_nodes(&body));
                }
            }
            if all_nodes.is_empty() {
                // Try direct URIs from article page
                return extract_uris_from_html(&article_html)
                    .into_iter()
                    .filter_map(|uri| {
                        let node = ProxyNode::parse(&uri).ok()?;
                        let name = node_display_name(&node);
                        Some((name, uri))
                    })
                    .collect();
            }
        }
    }

    all_nodes
}

/// Extract HTTP URLs from HTML that end with the given suffix.
fn extract_uris_from_html_by_ext(html: &str, suffix: &str) -> Vec<String> {
    let mut urls = Vec::new();
    // Match href="..." patterns
    let mut pos = 0;
    while let Some(idx) = html[pos..].find("href=\"") {
        let start = pos + idx + 6;
        if let Some(end) = html[start..].find('"') {
            let url = &html[start..start + end];
            if url.contains(suffix) {
                let full = if url.starts_with("http") {
                    url.to_string()
                } else if url.starts_with('/') {
                    // Resolve relative URL — extract origin from any http URL in the page
                    if let Some(origin_start) = html.find("https://") {
                        let origin_end = html[origin_start + 8..]
                            .find('/')
                            .map(|e| origin_start + 8 + e)
                            .unwrap_or(origin_start + 8);
                        format!("{}{}", &html[origin_start..origin_end], url)
                    } else {
                        pos = start + end;
                        continue;
                    }
                } else {
                    pos = start + end;
                    continue;
                };
                if !urls.contains(&full) {
                    urls.push(full);
                }
            }
            pos = start + end;
        } else {
            break;
        }
    }
    urls
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
        assert_eq!(result[0].0, "trojan-example.com:443");
    }
}
