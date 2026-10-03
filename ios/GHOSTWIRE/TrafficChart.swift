import Charts
import SwiftUI

/// Traffic bars like the web UI: one blue bar per bucket (total), or a blue
/// download and orange upload bar side by side (pair). Touch a bar to see
/// its values in the line above the chart.
struct TrafficChart: View {
    enum Mode { case total, pair }

    let points: [StatPoint]
    let range: String
    let mode: Mode
    @State private var selected: Date?

    private var unit: Calendar.Component { range == "24h" ? .hour : .day }

    private var selectedPoint: StatPoint? {
        guard let selected else { return nil }
        return points.first { Calendar.current.isDate($0.date, equalTo: selected, toGranularity: unit) }
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            readout
                .font(.footnote)
                .foregroundStyle(Color.gwText2)
                .frame(minHeight: 18, alignment: .leading)
            Chart {
                ForEach(points) { p in
                    if mode == .pair {
                        BarMark(x: .value("Time", p.date, unit: unit), y: .value("Bytes", Double(p.down)))
                            .foregroundStyle(by: .value("Series", "Download"))
                            .position(by: .value("Series", "Download"))
                            .cornerRadius(3)
                        BarMark(x: .value("Time", p.date, unit: unit), y: .value("Bytes", Double(p.up)))
                            .foregroundStyle(by: .value("Series", "Upload"))
                            .position(by: .value("Series", "Upload"))
                            .cornerRadius(3)
                    } else {
                        BarMark(x: .value("Time", p.date, unit: unit), y: .value("Bytes", Double(p.down + p.up)))
                            .foregroundStyle(by: .value("Series", "Download"))
                            .cornerRadius(3)
                            .opacity(selectedPoint == nil || selectedPoint == p ? 1 : 0.45)
                    }
                }
            }
            .chartForegroundStyleScale(["Download": Color.gwDown, "Upload": Color.gwUp])
            .chartLegend(.hidden)
            .chartYAxis {
                AxisMarks(position: .leading, values: .automatic(desiredCount: 3)) { v in
                    AxisGridLine()
                    AxisValueLabel {
                        if let b = v.as(Double.self) { Text(fmtBytes(Int64(b))).font(.caption2) }
                    }
                }
            }
            .chartXAxis {
                AxisMarks(values: .automatic(desiredCount: 4)) { _ in
                    AxisValueLabel(format: range == "24h" ? .dateTime.hour() : .dateTime.day().month(.abbreviated))
                }
            }
            .chartXSelection(value: $selected)
            .frame(height: 180)
        }
    }

    @ViewBuilder private var readout: some View {
        if let p = selectedPoint {
            let label = pointLabel(p, range: range)
            if mode == .pair {
                Text("\(label) · Download **\(fmtBytes(p.down))** · Upload **\(fmtBytes(p.up))**")
            } else {
                Text("**\(fmtBytes(p.down + p.up))** · \(label)")
            }
        } else if mode == .total, let peak = points.max(by: { $0.down + $0.up < $1.down + $1.up }), peak.down + peak.up > 0 {
            Text("**\(fmtBytes(peak.down + peak.up))** · peak, \(pointLabel(peak, range: range))")
        } else {
            Text("Touch a bar to see its values.")
        }
    }
}

/// Legend with totals for the pair chart.
struct TrafficTotals: View {
    let points: [StatPoint]
    var body: some View {
        let down = points.reduce(0) { $0 + $1.down }
        let up = points.reduce(0) { $0 + $1.up }
        HStack(spacing: 16) {
            HStack(spacing: 6) {
                RoundedRectangle(cornerRadius: 3).fill(Color.gwDown).frame(width: 12, height: 12)
                Text("Download **\(fmtBytes(down))**")
            }
            HStack(spacing: 6) {
                RoundedRectangle(cornerRadius: 3).fill(Color.gwUp).frame(width: 12, height: 12)
                Text("Upload **\(fmtBytes(up))**")
            }
        }
        .font(.footnote)
        .foregroundStyle(Color.gwText2)
    }
}
