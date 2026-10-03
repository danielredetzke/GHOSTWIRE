import Foundation

// Types mirror the JSON of GHOSTWIRE's /api/v1. Traffic is from the peer's
// point of view: down is what the peer downloaded, up what it uploaded.

nonisolated struct Me: Decodable {
    let name: String
    let isAdmin: Bool
    let scope: String
    let version: String
}

nonisolated struct HealthCheck: Decodable, Hashable {
    let name: String
    let ok: Bool
    let detail: String
}

nonisolated struct PeerCounts: Decodable {
    let total, enabled, online, disabled, never: Int
}

nonisolated struct Traffic: Decodable {
    let down: Int64
    let up: Int64
}

nonisolated struct Status: Decodable {
    let version: String
    let interface: String
    let listenPort: Int
    let endpoint: String
    let ipv4: String
    let ipv6: String
    let ipv6Enabled: Bool
    let capacity: Int
    let started: Date
    let healthy: Bool
    let checks: [HealthCheck]
    let peers: PeerCounts
    let traffic24h: Traffic
    let traffic30d: Traffic
    let topPeer30d: String
}

nonisolated struct StatPoint: Decodable, Identifiable, Hashable {
    let t: Int64
    let down: Int64
    let up: Int64
    var id: Int64 { t }
    var date: Date { Date(timeIntervalSince1970: TimeInterval(t)) }
}

nonisolated struct StatsResponse: Decodable {
    let range: String
    let points: [StatPoint]
}

nonisolated struct PeerStats: Decodable, Hashable {
    let online: Bool
    let lastHandshake: Date?
    let endpoint: String
    let down24h, up24h, down30d, up30d, downTotal, upTotal: Int64
    let location: GeoInfo?
}

/// Country and network of an address, from the server's DB-IP lookup.
nonisolated struct GeoInfo: Decodable, Hashable {
    let country: String?
    let countryName: String?
    let asn: Int?
    let network: String?

    /// "Germany · Deutsche Telekom AG" or "Local network".
    var label: String {
        [countryName ?? country, network].compactMap { $0 }.filter { !$0.isEmpty }.joined(separator: " · ")
    }
}

/// One row of a peer's connection history.
nonisolated struct ConnSession: Decodable, Identifiable, Hashable {
    let start: Date
    let end: Date
    let open: Bool
    let seconds: Int64
    let endpoint: String
    let ip: String
    let geo: GeoInfo?
    let down: Int64
    let up: Int64
    var id: String { "\(start.timeIntervalSince1970)-\(ip)" }
}

nonisolated struct SessionsResponse: Decodable {
    let sessions: [ConnSession]
}

nonisolated struct Peer: Decodable, Identifiable, Hashable {
    let id: String
    let name: String
    let note: String
    let enabled: Bool
    let publicKey: String
    let hasPresharedKey: Bool
    let ipv4: String
    let ipv6: String?
    let dns: [String]?          // nil = server default
    let allowedIPs: [String]?   // nil = server default
    let keepalive: Int?         // nil = server default
    let effectiveDNS: [String]
    let effectiveAllowedIPs: [String]
    let effectiveKeepalive: Int
    let created: Date
    let configIssued: Date?
    let setup: SetupInfo?       // pending setup link, nil if none
    let stats: PeerStats
}

/// A pending setup link as peer lists show it. The link itself is not in it.
nonisolated struct SetupInfo: Decodable, Hashable {
    let expires: Date
    let expired: Bool
    let pinRequired: Bool
    let pinFails: Int
}

nonisolated struct PeerList: Decodable {
    let peers: [Peer]
    let capacity: Int
    let network: String
}

nonisolated struct PeerResult: Decodable {
    let peer: Peer
    let applyError: String
}

/// A freshly issued client config. The private key exists only here.
nonisolated struct IssuedConfig: Decodable, Identifiable {
    let peer: Peer
    let config: String
    let qr: String?
    let includesPrivateKey: Bool
    let applyError: String
    var id: String { peer.id + peer.publicKey }
}

/// A setup link for the admin to send. It sets up a device once.
nonisolated struct SetupSecret: Decodable, Hashable {
    let url: String
    let path: String
    let pin: String?
    let expires: Date
    let qr: String
}

/// The answer to creating a peer or issuing a config with a setup link.
nonisolated struct LinkCreated: Decodable {
    let peer: Peer
    let setup: SetupSecret
    let applyError: String
}

/// A config shown now, or a setup link to send.
enum IssueOutcome: Identifiable {
    case config(IssuedConfig)
    case link(LinkCreated)

    var id: String {
        switch self {
        case .config(let c): c.id
        case .link(let l): l.setup.path
        }
    }

    var peer: Peer {
        switch self {
        case .config(let c): c.peer
        case .link(let l): l.peer
        }
    }

    var applyError: String {
        switch self {
        case .config(let c): c.applyError
        case .link(let l): l.applyError
        }
    }
}

nonisolated struct PeerOnly: Decodable {
    let peer: Peer
}

nonisolated struct ClientDefaults: Codable, Equatable {
    var dns: [String]
    var allowedIPs: [String]
    var keepalive: Int
}

/// Server settings as GET/PATCH /server use them.
nonisolated struct ServerConfig: Codable, Equatable {
    var interface: String
    var publicKey: String
    var keyCreated: Date
    var listenPort: Int
    var mtu: Int
    var ipv4: String
    var ipv6: String
    var ipv6Enabled: Bool
    var endpoint: String
    var endpointPort: Int
    var uplinkV4: String
    var uplinkV6: String
    var detectedUplinkV4: String
    var detectedUplinkV6: String
    var nat: Bool
    var peerToPeer: Bool
    var lanAccess: Bool
    var openPort: Bool
    var clientDefaults: ClientDefaults

    var networks: [String] { ipv6Enabled ? [ipv4, ipv6] : [ipv4] }
}

nonisolated struct ServerResult: Decodable {
    let server: ServerConfig
    let applyError: String
    let reissueNeeded: Bool?
}

nonisolated struct TLSSettings: Codable, Equatable {
    var mode: String
    var domain: String?
    var email: String?
    var staging: Bool?
    var certFile: String?
    var keyFile: String?
}

nonisolated struct WebSettings: Codable, Equatable {
    var listen: String
    var httpListen: String
    var tls: TLSSettings
    var sessionHours: Int
}

nonisolated struct LogSettings: Codable, Equatable {
    var level: String
    var maxSizeMB: Int
    var maxFiles: Int
}

nonisolated struct StatsSettings: Codable, Equatable {
    var hourlyHours: Int
    var dailyDays: Int
    var geoip: Bool?
}

nonisolated struct GeoStatus: Decodable {
    let enabled: Bool
    let updated: Date?
}

nonisolated struct AppSettings: Decodable {
    var web: WebSettings
    var log: LogSettings
    var stats: StatsSettings
    var adminUsername: String
    var fingerprint: String
    var logPath: String
    var geo: GeoStatus?
}

nonisolated struct SettingsResult: Decodable {
    let ok: Bool
    let restartRequired: Bool
}

nonisolated struct ApplyResult: Decodable {
    let applyError: String?
}

nonisolated struct DetectedIP: Decodable {
    let ip: String
}
