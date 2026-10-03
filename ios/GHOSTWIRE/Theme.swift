import SwiftUI
import UIKit

// Colours and components shared by all screens. They follow the web UI:
// ink, a light ground, white cards with hairlines, blue for downloads and
// orange for uploads. Dark mode uses the same roles, re-stepped.

nonisolated func uiColor(_ hex: UInt32) -> UIColor {
    UIColor(red: CGFloat((hex >> 16) & 0xFF) / 255,
            green: CGFloat((hex >> 8) & 0xFF) / 255,
            blue: CGFloat(hex & 0xFF) / 255, alpha: 1)
}

// nonisolated: UIKit resolves dynamic colours on SwiftUI's render thread, so
// the provider closure must not be bound to the main actor (that crashes).
nonisolated extension Color {
    init(hex: UInt32) { self.init(uiColor: uiColor(hex)) }

    static func dynamic(_ light: UInt32, _ dark: UInt32) -> Color {
        Color(uiColor: UIColor { $0.userInterfaceStyle == .dark ? uiColor(dark) : uiColor(light) })
    }

    static let gwGround = dynamic(0xF4F4F1, 0x0F0F10)
    static let gwSurface = dynamic(0xFFFFFF, 0x1C1D21)
    static let gwLine = dynamic(0xE3E3DE, 0x2C2D32)
    static let gwText = dynamic(0x16171A, 0xF2F2EE)
    static let gwText2 = dynamic(0x5B5C61, 0xA9AAA5)
    static let gwAccent = dynamic(0x16171A, 0xF2F2EE)
    static let gwBadge = dynamic(0xEFEFEB, 0x2A2B31)
    static let gwWarnBg = dynamic(0xFDF0E1, 0x3A2A16)
    static let gwWarnInk = dynamic(0x7A3D00, 0xF3C38B)
    static let gwErrBg = dynamic(0xFBEFEE, 0x3A1C1C)
    static let gwErrInk = dynamic(0xB4232A, 0xF2A7A3)
    static let gwDown = Color(hex: 0x2A78D6)
    static let gwUp = Color(hex: 0xEB6834)
    static let gwGood = Color(hex: 0x0CA30C)
    static let gwBad = Color(hex: 0xD03B3B)
    static let gwSumi = Color(hex: 0x1B1B1D)
    static let gwShu = Color(hex: 0xC8372D)
}

extension Font {
    static func mono(_ style: Font.TextStyle = .body, weight: Font.Weight = .regular) -> Font {
        .system(style, design: .monospaced).weight(weight)
    }
}

struct CardModifier: ViewModifier {
    func body(content: Content) -> some View {
        content
            .padding(16)
            .frame(maxWidth: .infinity, alignment: .leading)
            .background(Color.gwSurface, in: RoundedRectangle(cornerRadius: 12))
            .overlay(RoundedRectangle(cornerRadius: 12).stroke(Color.gwLine))
    }
}

extension View {
    func card() -> some View { modifier(CardModifier()) }

    /// Forms and lists on the app's ground colour instead of system grey.
    func groundBackground() -> some View {
        scrollContentBackground(.hidden).background(Color.gwGround)
    }
}

/// A full-width primary button in ink, like the web UI's primary buttons.
struct PrimaryButtonStyle: ButtonStyle {
    @Environment(\.isEnabled) private var enabled
    func makeBody(configuration: Configuration) -> some View {
        configuration.label
            .font(.body.weight(.semibold))
            .frame(maxWidth: .infinity, minHeight: 48)
            .foregroundStyle(Color.gwGround)
            .background(Color.gwAccent.opacity(configuration.isPressed ? 0.8 : 1), in: RoundedRectangle(cornerRadius: 10))
            .opacity(enabled ? 1 : 0.5)
    }
}

struct SecondaryButtonStyle: ButtonStyle {
    var ink = Color.gwText
    func makeBody(configuration: Configuration) -> some View {
        configuration.label
            .font(.body.weight(.medium))
            .frame(maxWidth: .infinity, minHeight: 48)
            .foregroundStyle(ink)
            .background(Color.gwSurface.opacity(configuration.isPressed ? 0.7 : 1), in: RoundedRectangle(cornerRadius: 10))
            .overlay(RoundedRectangle(cornerRadius: 10).stroke(Color.gwLine))
    }
}

// MARK: - Small components

struct Notice: View {
    let text: String
    var isError = false
    var body: some View {
        Text(text)
            .font(.footnote)
            .foregroundStyle(isError ? Color.gwErrInk : Color.gwWarnInk)
            .padding(12)
            .frame(maxWidth: .infinity, alignment: .leading)
            .background(isError ? Color.gwErrBg : Color.gwWarnBg, in: RoundedRectangle(cornerRadius: 10))
    }
}

enum PeerState {
    case online(Date), offline(Date), never, disabled, waiting, noConfig

    init(_ p: Peer) {
        if !p.enabled { self = .disabled }
        else if p.publicKey.isEmpty { self = p.setup.map { !$0.expired } == true ? .waiting : .noConfig }
        else if let h = p.stats.lastHandshake { self = p.stats.online ? .online(h) : .offline(h) }
        else { self = .never }
    }

    var label: String {
        switch self {
        case .online(let d): "Online · " + ago(d)
        case .offline(let d): "Offline · " + ago(d)
        case .never: "Never connected"
        case .disabled: "Disabled"
        case .waiting: "Waiting for setup"
        case .noConfig: "No config yet"
        }
    }

    var key: String {
        switch self {
        case .online: "online"
        case .offline, .never, .waiting, .noConfig: "offline"
        case .disabled: "disabled"
        }
    }
}

struct StatusDot: View {
    let state: PeerState
    var body: some View {
        switch state {
        case .online: Circle().fill(Color.gwGood).frame(width: 8, height: 8)
        case .offline: Circle().fill(Color.gray).frame(width: 8, height: 8)
        case .never, .noConfig: Circle().stroke(Color.gray, lineWidth: 1.5).frame(width: 8, height: 8)
        case .waiting: Circle().fill(Color.gwUp).frame(width: 8, height: 8)
        case .disabled: Circle().fill(Color.gwBad).frame(width: 8, height: 8)
        }
    }
}

struct StatusBadge: View {
    let state: PeerState
    var body: some View {
        HStack(spacing: 6) {
            StatusDot(state: state)
            Text(state.label)
        }
        .font(.caption.weight(.medium))
        .foregroundStyle(Color.gwText)
        .padding(.horizontal, 10)
        .padding(.vertical, 4)
        .background(Color.gwBadge, in: Capsule())
    }
}

struct Tile: View {
    let title: String
    let value: String
    var suffix: String? = nil
    var dot: Color? = nil
    let sub: String
    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            Text(title).font(.footnote).foregroundStyle(Color.gwText2)
            HStack(alignment: .firstTextBaseline, spacing: 6) {
                if let dot { Circle().fill(dot).frame(width: 10, height: 10) }
                Text(value).font(.title2.weight(.semibold)).minimumScaleFactor(0.7).lineLimit(1)
                if let suffix { Text(suffix).font(.body.weight(.medium)).foregroundStyle(Color.gwText2) }
            }
            Text(sub).font(.caption).foregroundStyle(Color.gwText2).lineLimit(2)
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
        .card()
    }
}

/// A label/value row for read-only details.
struct KV: View {
    let key: String
    let value: String
    var mono = false
    var body: some View {
        VStack(alignment: .leading, spacing: 2) {
            Text(key).font(.caption).foregroundStyle(Color.gwText2)
            Text(value)
                .font(mono ? .mono(.footnote) : .footnote)
                .textSelection(.enabled)
        }
        .frame(maxWidth: .infinity, alignment: .leading)
    }
}

struct SectionTitle: View {
    let text: String
    var body: some View { Text(text).font(.headline) }
}
