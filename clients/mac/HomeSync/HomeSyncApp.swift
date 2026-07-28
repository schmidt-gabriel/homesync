import HomeSyncKit
import SwiftUI

@main
struct HomeSyncApp: App {
    @State private var model: AppModel

    init() {
        let model = AppModel()

        // Started here, not from the menu's `.task`. With
        // `.menuBarExtraStyle(.window)` the content view is only built when the
        // popover is first opened, so starting there would leave a sync app
        // that does not sync until you look at it.
        model.start()

        _model = State(initialValue: model)
    }

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
}
