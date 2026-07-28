import HomeSyncKit
import SwiftUI

/// What drops down from the menu bar icon.
struct MenuBarView: View {
    @Bindable var model: AppModel
    @Environment(\.openSettings) private var openSettings

    var body: some View {
        VStack(alignment: .leading, spacing: 0) {
            header

            Divider().padding(.vertical, 8)

            if !model.conflicts.isEmpty {
                conflictList
            }

            actions
        }
        .padding(12)
        .frame(width: 300)
        .task {
            // Starting here rather than in the App initialiser means the engine
            // comes up once the scene exists, and comes back up after settings
            // change without any extra wiring.
            if !model.isConfigured { model.start() }
        }
    }

    private var header: some View {
        HStack(alignment: .top, spacing: 10) {
            Image(systemName: model.state.symbolName)
                .font(.title2)
                .foregroundStyle(model.state.needsAttention ? .orange : .green)
                .frame(width: 24)

            VStack(alignment: .leading, spacing: 2) {
                Text("HomeSync")
                    .font(.headline)

                Text(model.state.summary)
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    .fixedSize(horizontal: false, vertical: true)

                if case .syncing(let progress) = model.state,
                   let progress, let fraction = progress.fraction {
                    ProgressView(value: fraction)
                        .progressViewStyle(.linear)
                        .frame(maxWidth: 190)
                        .padding(.top, 3)
                }

                if model.isConfigured, case .idle = model.state {
                    // HTTP on a home network is the expected setup, not a
                    // problem, so it belongs here as a quiet note about which
                    // connection is in use — not as a warning competing with
                    // the sync status.
                    Text(model.isInsecure
                        ? "\(model.fileCount) files · local network"
                        : "\(model.fileCount) files · encrypted")
                        .font(.caption)
                        .foregroundStyle(.tertiary)
                }
            }

            Spacer()
        }
    }

    /// Conflicts get their own section rather than a log line: a conflict is
    /// the one thing here that needs a person to decide something.
    private var conflictList: some View {
        VStack(alignment: .leading, spacing: 6) {
            HStack {
                Label(
                    "^[\(model.conflicts.count) conflict](inflect: true)",
                    systemImage: "doc.on.doc")
                    .font(.caption.weight(.semibold))

                Spacer()

                Button("Clear") { model.clearConflicts() }
                    .buttonStyle(.link)
                    .font(.caption)
            }

            Text("Both copies were kept. Open them, decide, and delete the one you do not want.")
                .font(.caption2)
                .foregroundStyle(.secondary)
                .fixedSize(horizontal: false, vertical: true)

            ForEach(model.conflicts.suffix(5), id: \.self) { path in
                Button {
                    model.revealInFinder(path)
                } label: {
                    Text(path)
                        .font(.caption.monospaced())
                        .lineLimit(1)
                        .truncationMode(.middle)
                        .frame(maxWidth: .infinity, alignment: .leading)
                }
                .buttonStyle(.plain)
                .foregroundStyle(.tint)
            }
        }
        .padding(.bottom, 10)
    }

    private var actions: some View {
        VStack(spacing: 2) {
            MenuButton("Sync Now", systemImage: "arrow.clockwise") { model.syncNow() }
                .disabled(!model.isConfigured)

            MenuButton("Open Sync Folder", systemImage: "folder") { model.openSyncFolder() }

            MenuButton("Settings…", systemImage: "gearshape") {
                openSettings()
                NSApp.activate(ignoringOtherApps: true)
            }

            Divider().padding(.vertical, 4)

            MenuButton("Quit HomeSync", systemImage: "power") {
                NSApplication.shared.terminate(nil)
            }

            // Small and out of the way, but always reachable: the answer to
            // "which version are you running" should not require the Finder.
            Text("Version \(AppModel.version)")
                .font(.caption2)
                .foregroundStyle(.tertiary)
                .frame(maxWidth: .infinity, alignment: .center)
                .padding(.top, 6)
                .textSelection(.enabled)
        }
    }
}

/// A row that looks like a menu item but works inside a `.window`-style
/// `MenuBarExtra`, where real `Menu` items are not available.
private struct MenuButton: View {
    let title: String
    let systemImage: String
    let action: () -> Void

    @State private var isHovering = false

    init(_ title: String, systemImage: String, action: @escaping () -> Void) {
        self.title = title
        self.systemImage = systemImage
        self.action = action
    }

    var body: some View {
        Button(action: action) {
            Label(title, systemImage: systemImage)
                .frame(maxWidth: .infinity, alignment: .leading)
                .padding(.horizontal, 6)
                .padding(.vertical, 5)
                .contentShape(Rectangle())
                .background(
                    isHovering ? Color.primary.opacity(0.08) : .clear,
                    in: RoundedRectangle(cornerRadius: 5))
        }
        .buttonStyle(.plain)
        .onHover { isHovering = $0 }
    }
}
