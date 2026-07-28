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
                    status

                    Spacer()

                    Button("Apply") {
                        Task { await model.applyServerSettings(url: draftURL, token: draftToken) }
                    }
                    .keyboardShortcut(.defaultAction)
                    .disabled(!hasChanges || isChecking)
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
        .onChange(of: draftURL) { model.clearConnectionCheck() }
        .onChange(of: draftToken) { model.clearConnectionCheck() }
    }

    /// Apply stays disabled while the fields still match what is saved: with
    /// nothing to apply, pressing it would restart the engine for no reason and
    /// tell the user nothing.
    private var hasChanges: Bool {
        let url = draftURL.trimmingCharacters(in: .whitespaces)
        let token = draftToken.trimmingCharacters(in: .whitespaces)

        guard !url.isEmpty, !token.isEmpty else { return false }
        return url != model.serverURL || token != model.token
    }

    private var isChecking: Bool { model.connectionCheck == .checking }

    @ViewBuilder
    private var status: some View {
        switch model.connectionCheck {
        case .none:
            EmptyView()

        case .checking:
            HStack(spacing: 6) {
                ProgressView().controlSize(.small)
                Text("Connecting…")
            }
            .font(.caption)
            .foregroundStyle(.secondary)

        case .connected:
            Label("Connected", systemImage: "checkmark.circle.fill")
                .font(.caption)
                .foregroundStyle(.green)

        case .failed(let reason):
            Label(reason, systemImage: "exclamationmark.triangle.fill")
                .font(.caption)
                .foregroundStyle(.red)
                .fixedSize(horizontal: false, vertical: true)
        }
    }
}

private struct GeneralSettings: View {
    @Bindable var model: AppModel
    @State private var draftFolder = ""

    var body: some View {
        Form {
            Section {
                // Typed as well as picked. A panel cannot reach a folder that
                // does not exist yet, or one on a volume that is not mounted
                // right now, and both are reasonable things to configure.
                TextField(
                    "Sync folder",
                    text: $draftFolder,
                    prompt: Text(AppModel.defaultFolder))
                    .font(.caption.monospaced())

                HStack {
                    Button("Choose…") { chooseFolder() }
                    Button("Use Default") { draftFolder = AppModel.defaultFolder }
                        .disabled(expanded == AppModel.defaultFolder)

                    Spacer()

                    Button("Move") { apply() }
                        .disabled(!hasChanges)
                }

                if hasChanges {
                    Text("""
                        Changing this starts over: HomeSync downloads everything into the \
                        new folder. The old one is left exactly as it is.
                        """)
                        .font(.caption)
                        .foregroundStyle(.secondary)
                        .fixedSize(horizontal: false, vertical: true)
                }
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

            Section {
                LabeledContent("Version") {
                    Text(AppModel.version)
                        .font(.caption.monospaced())
                        .textSelection(.enabled)
                }
            } header: {
                Text("About")
            }
        }
        .formStyle(.grouped)
        .onAppear { draftFolder = model.syncFolder }
    }

    /// A typed path may well use `~`, and may have picked up whitespace from a
    /// copy and paste.
    private var expanded: String {
        (draftFolder.trimmingCharacters(in: .whitespaces) as NSString).expandingTildeInPath
    }

    private var hasChanges: Bool {
        !expanded.isEmpty && expanded.hasPrefix("/") && expanded != model.syncFolder
    }

    private func apply() {
        guard hasChanges else { return }
        model.syncFolder = expanded
        draftFolder = expanded
        model.restart()
    }

    private func chooseFolder() {
        let panel = NSOpenPanel()
        panel.canChooseDirectories = true
        panel.canChooseFiles = false
        panel.canCreateDirectories = true
        panel.directoryURL = URL(fileURLWithPath: model.syncFolder)
        panel.prompt = "Select"

        guard panel.runModal() == .OK, let url = panel.url else { return }
        draftFolder = url.path
    }
}
