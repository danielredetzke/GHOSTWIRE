import Foundation

/// Bytes in decimal units, as the web UI shows them: "11.2 MB".
nonisolated func fmtBytes(_ n: Int64) -> String {
    let units = ["B", "KB", "MB", "GB", "TB", "PB"]
    var v = Double(n)
    var i = 0
    while v >= 1000 && i < units.count - 1 {
        v /= 1000
        i += 1
    }
    let s: String
    if i == 0 { s = String(Int(v)) }
    else if v < 10 { s = String(format: "%.2f", v) }
    else if v < 100 { s = String(format: "%.1f", v) }
    else { s = String(Int(v.rounded())) }
    return s + " " + units[i]
}

func ago(_ date: Date?) -> String {
    guard let date else { return "never" }
    let s = max(0, Date().timeIntervalSince(date))
    switch s {
    case ..<60: return "\(Int(s)) s ago"
    case ..<3600: return "\(Int(s / 60)) min ago"
    case ..<86400: return "\(Int(s / 3600)) h ago"
    default: return "\(Int(s / 86400)) d ago"
    }
}

func fmtDate(_ date: Date?) -> String {
    guard let date, date.timeIntervalSince1970 > 0 else { return "–" }
    return date.formatted(date: .abbreviated, time: .omitted)
}

/// Label of a chart point: "3 h ago" for hours, "Sat 3 Oct" for days.
func pointLabel(_ p: StatPoint, range: String) -> String {
    if range == "24h" {
        let h = Int((Date().timeIntervalSince(p.date) / 3600).rounded(.down))
        return h <= 0 ? "This hour" : "\(h) h ago"
    }
    return p.date.formatted(.dateTime.weekday(.abbreviated).day().month(.abbreviated))
}

/// Parses Go's RFC 3339 times, which carry up to nine fractional digits.
nonisolated func parseGoDate(_ s: String) -> Date? {
    var str = s
    if let dot = str.firstIndex(of: "."),
       let end = str[dot...].firstIndex(where: { $0 == "Z" || $0 == "+" || $0 == "-" }) {
        let frac = str[str.index(after: dot)..<end]
        let ms = String(frac.prefix(3)).padding(toLength: 3, withPad: "0", startingAt: 0)
        str = String(str[..<dot]) + "." + ms + String(str[end...])
    }
    let f = ISO8601DateFormatter()
    f.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
    if let d = f.date(from: str) { return d }
    f.formatOptions = [.withInternetDateTime]
    return f.date(from: s)
}

/// Splits "a, b,c" into ["a", "b", "c"].
func splitList(_ s: String) -> [String] {
    s.split(separator: ",").map { $0.trimmingCharacters(in: .whitespaces) }.filter { !$0.isEmpty }
}

/// Formats one JSON log record like the web UI: time, level, message, details.
func formatLogLine(_ rec: [String: Any]) -> String {
    var ts = rec["time"] as? String ?? ""
    if let d = parseGoDate(ts) {
        ts = d.formatted(.dateTime.year().month(.twoDigits).day(.twoDigits).hour(.twoDigits(amPM: .omitted)).minute(.twoDigits).second(.twoDigits))
    }
    let level = (rec["level"] as? String ?? "").padding(toLength: 5, withPad: " ", startingAt: 0)
    let msg = rec["msg"] as? String ?? ""
    let rest = rec.keys.filter { !["time", "level", "msg", "audit"].contains($0) }.sorted().map { k -> String in
        let v = rec[k]
        if let s = v as? String { return "\(k)=\(s)" }
        if let v, let d = try? JSONSerialization.data(withJSONObject: v, options: [.fragmentsAllowed]), let s = String(data: d, encoding: .utf8) {
            return "\(k)=\(s)"
        }
        return "\(k)=?"
    }.joined(separator: " ")
    return "\(ts)  \(level)  \(msg)" + (rest.isEmpty ? "" : "  " + rest)
}
