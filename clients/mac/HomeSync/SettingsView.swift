import HomeSyncKit
import SwiftUI

struct SettingsView: View {
    @Bindable var model: AppModel

    var body: some View {
        TabView {
            ServerSettings(model: model)
                .tabItem { Label("Server", systemImage: "server.rack") }

            GeneralSettings(model: model)
                .tabItem { Label("General", systemImage: "gearshape") }
        }
        .frame(width: 460)
    }
}

private struct ServerSettings: View {
    @Bindable var model: AppModel
    @State private var draftURL = ""
    @State private var draftToken = ""

    var body: some View {
        Form {
            Section {
                TextField("Server URL", text: $draftURL, prompt: Text("https://homelab.local:8420"))
                    .textContentType(.URL)

                SecureField("Device token", text: $draftToken)

                Text("""
                    Create a token on the server with `homesync device add <name>`, or from its \
                    admin page. It is shown once and cannot be recovered, so paste it now.
                    """)
                    .font(.caption)
                    .foregroundStyle(.secondary)
            } header: {
                Text("Connection")
            }

            // HomeSync is built for a home network, so plain HTTP is the
            // ordinary case and gets stated plainly rather than flagged. The
            // one thing worth saying is where the assumption stops holding.
            if !draftURL.isEmpty, !draftURL.lowercased().hasPrefix("https") {
                Label(
                    "Unencrypted, which is fine on your own network. Use https:// "
                        + "if this server is reachable from outside it.",
                    systemImage: "house")
                    .font(.caption)
                    .foregroundStyle(.secondary)
            }

            Section {
                HStack {
                    Spacer()
                    Button("Apply") {
                        model.serverURL = draftURL.trimmingCharacters(in: .whitespaces)
                        model.token = draftToken.trimmingCharacters(in: .whitespaces)
                        model.restart()
                    }
                    .keyboardShortcut(.defaultAction)
                    .disabled(draftURL.isEmpty || draftToken.isEmpty)
                }
            }
        }
        .formStyle(.grouped)
        .onAppear {
            // Edited in a draft so a half-typed URL never restarts the engine
            // on every keystroke.
            draftURL = model.serverURL
            draftToken = model.token
        }
    }
}

private struct GeneralSettings: View {
    @Bindable var model: AppModel
    @State private var isChoosingFolder = false

    var body: some View {
        Form {
            Section {
                LabeledContent("Sync folder") {
                    HStack {
                        Text(model.syncFolder)
                            .font(.caption.monospaced())
                            .lineLimit(1)
                            .truncationMode(.head)

                        Button("Choose…") { chooseFolder() }
                    }
                }

                Text("""
                    A symlink at ~/HomeSync points here, since \
                    ~/Library/CloudStorage is awkward to reach.
                    """)
                    .font(.caption)
                    .foregroundStyle(.secondary)
            } header: {
                Text("Files")
            }

            Section {
                Toggle("Start HomeSync at login", isOn: Binding(
                    get: { model.launchesAtLogin },
                    set: { model.launchesAtLogin = $0 }
                ))
            } header: {
                Text("Startup")
            }
        }
        .formStyle(.grouped)
    }

    private func chooseFolder() {
        let panel = NSOpenPanel()
        panel.canChooseDirectories = true
        panel.canChooseFiles = false
        panel.canCreateDirectories = true
        panel.directoryURL = URL(fileURLWithPath: model.syncFolder)
        panel.prompt = "Select"

        guard panel.runModal() == .OK, let url = panel.url else { return }
        model.syncFolder = url.path
        model.restart()
    }
}
