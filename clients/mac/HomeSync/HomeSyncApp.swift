import HomeSyncKit
import SwiftUI

@main
struct HomeSyncApp: App {
    @State private var model = AppModel()

    var body: some Scene {
        // MenuBarExtra is a Scene, so there is no NSStatusItem to manage by
        // hand. Combined with LSUIElement in Info.plist, the app has no Dock
        // icon and no main window: the menu bar is the whole interface.
        MenuBarExtra {
            MenuBarView(model: model)
        } label: {
            Image(systemName: model.state.symbolName)
                .accessibilityLabel("HomeSync: \(model.state.summary)")
        }
        .menuBarExtraStyle(.window)

        Settings {
            SettingsView(model: model)
        }
    }

    init() {
        // A @State property wrapper cannot be touched before the scene exists,
        // so the engine starts from the first body evaluation instead.
    }
}
