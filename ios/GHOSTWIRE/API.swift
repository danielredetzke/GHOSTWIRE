import CryptoKit
import Foundation

enum APIError: LocalizedError {
    case server(String)
    case unauthorized
    case badPairing(String)

    var errorDescription: String? {
        switch self {
        case .server(let m): m
        case .unauthorized: "This iPhone is no longer paired. Pair it again from Settings → Pair iOS app in the web interface."
        case .badPairing(let m): m
        }
    }
}

/// Accepts the server only if its certificate matches the fingerprint from
/// the pairing code. Without a fingerprint (Let's Encrypt), normal system
/// trust applies.
nonisolated final class PinningDelegate: NSObject, URLSessionDelegate, Sendable {
    let fingerprint: String

    init(fingerprint: String) {
        self.fingerprint = fingerprint.replacingOccurrences(of: ":", with: "").uppercased()
    }

    func urlSession(_ session: URLSession, didReceive challenge: URLAuthenticationChallenge) async
        -> (URLSession.AuthChallengeDisposition, URLCredential?) {
        guard challenge.protectionSpace.authenticationMethod == NSURLAuthenticationMethodServerTrust,
              let trust = challenge.protectionSpace.serverTrust,
              !fingerprint.isEmpty else {
            return (.performDefaultHandling, nil)
        }
        guard let chain = SecTrustCopyCertificateChain(trust) as? [SecCertificate], let leaf = chain.first else {
            return (.cancelAuthenticationChallenge, nil)
        }
        let digest = SHA256.hash(data: SecCertificateCopyData(leaf) as Data)
        let hex = digest.map { String(format: "%02X", $0) }.joined()
        return hex == fingerprint ? (.useCredential, URLCredential(trust: trust)) : (.cancelAuthenticationChallenge, nil)
    }
}

/// Client for GHOSTWIRE's /api/v1, authenticated with the paired API token.
final class API {
    let base: String
    private let token: String
    private let session: URLSession

    init(pairing p: Pairing) {
        var url = p.url.trimmingCharacters(in: .whitespacesAndNewlines)
        while url.hasSuffix("/") { url.removeLast() }
        base = url
        token = p.token
        let cfg = URLSessionConfiguration.ephemeral
        cfg.timeoutIntervalForRequest = 15
        session = URLSession(configuration: cfg, delegate: PinningDelegate(fingerprint: p.fingerprint), delegateQueue: nil)
    }

    static let decoder: JSONDecoder = {
        let d = JSONDecoder()
        d.dateDecodingStrategy = .custom { dec in
            let s = try dec.singleValueContainer().decode(String.self)
            guard let date = parseGoDate(s) else {
                throw DecodingError.dataCorrupted(.init(codingPath: dec.codingPath, debugDescription: "bad date \(s)"))
            }
            return date
        }
        return d
    }()

    /// Sends a request and returns the raw body. Body values of nil are sent
    /// as JSON null ("use the server default").
    func data(_ method: String, _ path: String, body: [String: Any?]? = nil) async throws -> Data {
        guard let url = URL(string: base + "/api/v1" + path) else { throw APIError.badPairing("The server address is not valid.") }
        var req = URLRequest(url: url)
        req.httpMethod = method
        req.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
        if let body {
            req.setValue("application/json", forHTTPHeaderField: "Content-Type")
            req.httpBody = try JSONSerialization.data(withJSONObject: body.mapValues { $0 ?? NSNull() })
        }
        let (data, resp) = try await session.data(for: req)
        let code = (resp as? HTTPURLResponse)?.statusCode ?? 0
        if code == 401 { throw APIError.unauthorized }
        guard (200..<300).contains(code) else {
            let obj = try? JSONSerialization.jsonObject(with: data) as? [String: Any]
            throw APIError.server(obj?["error"] as? String ?? "The server answered with HTTP \(code).")
        }
        return data
    }

    func get<T: Decodable>(_ path: String) async throws -> T {
        try Self.decoder.decode(T.self, from: try await data("GET", path))
    }

    func send<T: Decodable>(_ method: String, _ path: String, _ body: [String: Any?]? = nil) async throws -> T {
        try Self.decoder.decode(T.self, from: try await data(method, path, body: body))
    }
}
