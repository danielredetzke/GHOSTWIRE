import SwiftUI

/// The Kamon mark: a crest ring around the ghost, drawn from the same
/// 64×64 geometry as favicon.svg.
struct KamonMark: View {
    var size: CGFloat = 40

    var body: some View {
        Canvas(renderer: Self.draw)
            .frame(width: size, height: size)
            .accessibilityHidden(true)
    }

    // nonisolated: SwiftUI may render a Canvas on its background render
    // thread; a main-actor-bound renderer would crash there.
    nonisolated private static func draw(_ ctx: inout GraphicsContext, _ canvas: CGSize) {
        let s = canvas.width / 64
        let tile = CGRect(x: 0, y: 0, width: 64 * s, height: 64 * s)
        ctx.fill(Path(roundedRect: tile, cornerRadius: 14 * s), with: .color(.gwSumi))
        ctx.stroke(Path(roundedRect: tile.insetBy(dx: 0.5 * s, dy: 0.5 * s), cornerRadius: 13.5 * s),
                   with: .color(.white.opacity(0.2)), lineWidth: s)
        ctx.stroke(Path(ellipseIn: CGRect(x: 10 * s, y: 10 * s, width: 44 * s, height: 44 * s)),
                   with: .color(.white), lineWidth: 3.5 * s)

        // The ghost is scaled by 0.66 around (32, 33), as in the SVG.
        func p(_ x: CGFloat, _ y: CGFloat) -> CGPoint {
            CGPoint(x: (32 + (x - 32) * 0.66) * s, y: (33 + (y - 33) * 0.66) * s)
        }
        var ghost = Path()
        ghost.move(to: p(18, 50))
        ghost.addLine(to: p(18, 30))
        ghost.addRelativeArc(center: p(32, 30), radius: 14 * 0.66 * s,
                             startAngle: .degrees(180), delta: .degrees(180))
        ghost.addLine(to: p(46, 50))
        for (x, y) in [(41.3, 46.0), (36.7, 50.0), (32.0, 46.0), (27.3, 50.0), (22.7, 46.0)] {
            ghost.addLine(to: p(x, y))
        }
        ghost.closeSubpath()
        ctx.fill(ghost, with: .color(.white))

        let r = 3.2 * 0.66 * s
        for c in [p(27, 30), p(37, 30)] {
            ctx.fill(Path(ellipseIn: CGRect(x: c.x - r, y: c.y - r, width: 2 * r, height: 2 * r)), with: .color(.gwShu))
        }
    }
}

/// Mark, wordmark and katakana reading, as in the web UI.
struct Lockup: View {
    var size: CGFloat = 36

    var body: some View {
        HStack(spacing: size * 0.3) {
            KamonMark(size: size)
            VStack(alignment: .leading, spacing: 1) {
                Text("GHOSTWIRE")
                    .font(.system(size: size * 0.47, weight: .semibold, design: .monospaced))
                    .tracking(size * 0.03)
                Text("ゴーストワイヤー")
                    .font(.system(size: size * 0.27))
                    .tracking(size * 0.08)
                    .foregroundStyle(Color.gwText2)
            }
        }
        .accessibilityElement(children: .ignore)
        .accessibilityLabel("GHOSTWIRE")
    }
}
