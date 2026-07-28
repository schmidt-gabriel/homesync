import HomeSyncKit
import SwiftUI

/// The menu bar icon.
///
/// For most of the day this is the entire interface, so it has to carry the
/// state on its own: a glance at it should answer "is it working, and is
/// anything wrong" without opening anything.
struct MenuBarIcon: View {
    let state: SyncState

    var body: some View {
        switch state {
        case .syncing:
            // A spinning arrow is the universal idiom for work in progress, and
            // motion is the one thing a still symbol cannot express.
            //
            // TimelineView rather than a repeating SwiftUI animation: a menu bar
            // label is redrawn by the system on its own terms, and an implicit
            // animation attached to it does not reliably keep running. Driving
            // the angle from a clock makes each frame a plain state change,
            // which does redraw.
            TimelineView(.periodic(from: .now, by: 1.0 / 20.0)) { context in
                Image(systemName: "arrow.triangle.2.circlepath")
                    .rotationEffect(.degrees(Self.angle(at: context.date)))
            }

        case .idle:
            Image(systemName: "checkmark.circle")

        case .paused:
            Image(systemName: "pause.circle")

        case .failed:
            Image(systemName: "exclamationmark.triangle")
        }
    }

    /// One full turn every two seconds, derived from the clock so the rotation
    /// is continuous across redraws rather than restarting from zero.
    static func angle(at date: Date) -> Double {
        let period = 2.0
        let progress = date.timeIntervalSinceReferenceDate.truncatingRemainder(dividingBy: period)
        return progress / period * 360
    }
}
