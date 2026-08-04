import HomeSyncKit
import SwiftUI

/// The menu bar icon.
///
/// For most of the day this is the entire interface, so it has to carry the
/// state on its own: a glance at it should answer "is it working, and is
/// anything wrong" without opening anything.
///
/// **Do not animate this by changing the image.** Assigning to a status item's
/// image re-rasterises the symbol through CoreUI every time, and it is far more
/// expensive than it looks. Driving a rotation from a timer at 20fps put ~98%
/// of the main thread inside `NSStatusBarButton.setImage:`, which froze the
/// menu and the Settings window outright — while making the icon look stuck,
/// because the main thread had no time left to present anything.
///
/// A symbol effect is different in kind: the animation runs inside the image
/// layer, and the image is assigned once.
struct MenuBarIcon: View {
    let state: SyncState

    var body: some View {
        Image(systemName: symbol)
            .symbolEffect(.pulse, options: .repeating, isActive: isSyncing)
    }

    private var isSyncing: Bool {
        if case .syncing = state { return true }
        return false
    }

    private var symbol: String {
        switch state {
        case .syncing: return "arrow.triangle.2.circlepath"
        case .idle: return "checkmark.circle"
        case .offline: return "wifi.slash"
        case .paused: return "pause.circle"
        case .failed: return "exclamationmark.triangle"
        }
    }
}
