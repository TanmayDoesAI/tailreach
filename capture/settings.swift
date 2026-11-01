// `tailreach settings` — the settings window. It talks to the running tailreach through
// its local control socket (owner-only Unix socket), never over the network.
import AppKit
import SwiftUI

// MARK: - Model

struct TrustedDevice: Decodable, Identifiable {
    let id: String
    let device: String
    let expires: String
}

struct LiveSession: Decodable, Identifiable {
    let id: Int
    let kind: String
    let viewer: String
    let started: String
}

struct GuestInvite: Decodable, Identifiable {
    let login: String
    let viewOnly: Bool
    let expires: String
    let used: String?
    var id: String { login }
}

/// The reply to "invite": the one-time code is shown once, here, and never stored.
struct NewInvite: Decodable {
    let login: String
    let code: String
    let address: String
    let machine: String
    let admin: String
    let viewOnly: Bool
}

struct ControlState: Decodable {
    var passwordEnabled = false
    var passwordSet = false
    var keepAwake = false
    var trusted: [TrustedDevice] = []
    var passkeys = 0
    var sessions: [LiveSession] = []
    var url = ""
    var owner = ""
    var managesLogin = true
    var invites: [GuestInvite]?
    var invited: NewInvite?
    var error: String?
}

final class ControlClient: ObservableObject {
    let path: String
    @Published var state = ControlState()
    @Published var message: String?
    @Published var offline = false
    @Published var pw1 = ""
    @Published var pw2 = ""
    @Published var guestLogin = ""
    @Published var guestHours = 24
    @Published var guestViewOnly = true
    @Published var lastInvite: NewInvite?
    @Published var guestMessage: String?
    private var timer: Timer?

    init(path: String) {
        self.path = path
        send(["op": "status"])
        timer = Timer.scheduledTimer(withTimeInterval: 2, repeats: true) { [weak self] _ in self?.send(["op": "status"]) }
    }

    func send(_ req: [String: Any], done: String? = nil) {
        DispatchQueue.global(qos: .userInitiated).async {
            let reply = self.roundTrip(req)
            DispatchQueue.main.async {
                guard let reply, let st = try? JSONDecoder().decode(ControlState.self, from: reply) else {
                    self.offline = true
                    return
                }
                self.offline = false
                let guestOp = ["invite", "uninvite"].contains(req["op"] as? String ?? "")
                if let err = st.error {
                    if guestOp { self.guestMessage = err } else { self.message = err }
                    return
                }
                self.state = st
                if let inv = st.invited {
                    self.lastInvite = inv
                    self.guestLogin = ""
                    self.guestMessage = nil
                } else if req["op"] as? String == "uninvite" {
                    self.lastInvite = nil
                    self.guestMessage = "Removed. Anything they had open was ended."
                }
                if let done { self.message = done }
            }
        }
    }

    func invite() {
        send(["op": "invite", "login": guestLogin.trimmingCharacters(in: .whitespaces), "hours": guestHours, "viewOnly": guestViewOnly])
    }

    /// One JSON line out, one JSON line back.
    private func roundTrip(_ req: [String: Any]) -> Data? {
        let fd = socket(AF_UNIX, SOCK_STREAM, 0)
        guard fd >= 0 else { return nil }
        defer { close(fd) }
        var addr = sockaddr_un()
        addr.sun_family = sa_family_t(AF_UNIX)
        let bytes = Array(path.utf8)
        guard bytes.count < MemoryLayout.size(ofValue: addr.sun_path) else { return nil }
        withUnsafeMutableBytes(of: &addr.sun_path) { raw in
            raw.copyBytes(from: bytes)
            raw[bytes.count] = 0
        }
        let ok = withUnsafePointer(to: &addr) {
            $0.withMemoryRebound(to: sockaddr.self, capacity: 1) {
                connect(fd, $0, socklen_t(MemoryLayout<sockaddr_un>.size)) == 0
            }
        }
        guard ok, var body = try? JSONSerialization.data(withJSONObject: req) else { return nil }
        body.append(0x0A)
        guard body.withUnsafeBytes({ write(fd, $0.baseAddress, $0.count) }) == body.count else { return nil }
        var out = Data()
        var buf = [UInt8](repeating: 0, count: 16384)
        while !out.contains(0x0A) {
            let n = read(fd, &buf, buf.count)
            if n <= 0 { break }
            out.append(buf, count: n)
        }
        return out.isEmpty ? nil : out
    }
}

// MARK: - Views

struct SettingsView: View {
    @ObservedObject var control: ControlClient

    var body: some View {
        Form {
            if control.offline {
                Section {
                    Label("tailreach isn't running on this Mac.", systemImage: "exclamationmark.triangle")
                        .foregroundStyle(.orange)
                }
            }

            Section {
                LabeledContent("Address") { Text(control.state.url).textSelection(.enabled) }
                LabeledContent("Tailscale account") { Text(control.state.owner) }
            } header: { Text("Access") } footer: {
                Text("Only devices signed in to this Tailscale account can reach it. Nothing is open on your Wi-Fi or the internet.")
            }

            if control.state.managesLogin {
                Section {
                    Toggle("Ask new devices for a password", isOn: Binding(
                        get: { control.state.passwordEnabled },
                        set: { control.send(["op": "enablePassword", "enabled": $0]) }))
                        .disabled(!control.state.passwordSet)
                    SecureField(control.state.passwordSet ? "New password" : "Password", text: $control.pw1)
                    SecureField("Confirm password", text: $control.pw2)
                    HStack {
                        if let m = control.message { Text(m).font(.callout).foregroundStyle(.secondary) }
                        Spacer()
                        Button(control.state.passwordSet ? "Change Password" : "Set Password") {
                            control.send(["op": "setPassword", "password": control.pw1], done: "Password saved. Every device will be asked again.")
                            control.pw1 = ""; control.pw2 = ""
                        }
                        .disabled(control.pw1.count < 8 || control.pw1 != control.pw2)
                    }
                } header: { Text("Device password") } footer: {
                    Text("Each phone, tablet or computer enters it once, then is trusted for 30 days. At least 8 characters.")
                }

                Section("Trusted devices") {
                    if control.state.trusted.isEmpty {
                        Text("None").foregroundStyle(.secondary)
                    }
                    ForEach(control.state.trusted) { t in
                        HStack {
                            VStack(alignment: .leading) {
                                Text(t.device)
                                Text("Trusted until \(shortDate(t.expires))").font(.caption).foregroundStyle(.secondary)
                            }
                            Spacer()
                            Button("Revoke") { control.send(["op": "revokeTrust", "id": t.id]) }
                        }
                    }
                    if control.state.trusted.count > 1 {
                        Button("Revoke All", role: .destructive) { control.send(["op": "revokeTrust", "id": "*"]) }
                    }
                }

                Section {
                    LabeledContent("Passkeys", value: "\(control.state.passkeys)")
                    Button("Remove All Passkeys", role: .destructive) { control.send(["op": "resetPasskeys"]) }
                        .disabled(control.state.passkeys == 0)
                } header: { Text("Face ID / passkeys") } footer: {
                    Text("A new device adds its passkey after entering the device password (or, with no password set, a code shown on this Mac).")
                }
            }

            if control.state.managesLogin {
                GuestsSection(control: control)
            }

            Section {
                Toggle("Keep this Mac reachable while plugged in", isOn: Binding(
                    get: { control.state.keepAwake },
                    set: { control.send(["op": "keepAwake", "enabled": $0]) }))
            } header: { Text("Availability") } footer: {
                Text("Stops the Mac sleeping on power so you can always reach it; the display still turns off. When it's locked, your phone can unlock it with the Mac password. After a restart, FileVault needs the password typed at the Mac once.")
            }

            Section("Live sessions") {
                if control.state.sessions.isEmpty {
                    Text("No one is viewing this Mac").foregroundStyle(.secondary)
                }
                ForEach(control.state.sessions) { s in
                    HStack {
                        Circle().fill(.red).frame(width: 8, height: 8)
                        Text(s.viewer)
                        Spacer()
                        Text(s.kind == "vnc" ? "VNC" : "Stream").font(.caption).foregroundStyle(.secondary)
                    }
                }
                Button("End All Sessions and Sign Everyone Out", role: .destructive) { control.send(["op": "stopSessions"]) }
            }

            Section {
                Text("Viewers can see this screen, move the pointer, type, scroll and use the clipboard. They can't run commands, open files or start anything in the background. The screen helper only runs while someone is connected, and a panel at the top right shows it.")
                    .font(.callout).foregroundStyle(.secondary)
            }
        }
        .formStyle(.grouped)
        .frame(width: 500, height: 680)
    }
}

/// Guests: someone else (a friend, a colleague) let in for a while, by their Tailscale login.
struct GuestsSection: View {
    @ObservedObject var control: ControlClient

    var body: some View {
        Section {
            if (control.state.invites ?? []).isEmpty {
                Text("No one else can get in").foregroundStyle(.secondary)
            }
            ForEach(control.state.invites ?? []) { g in
                HStack {
                    VStack(alignment: .leading) {
                        Text(g.login)
                        Text("\(g.viewOnly ? "View only" : "Can control") · until \(shortDateTime(g.expires))\(g.used == nil ? " · code not used yet" : "")")
                            .font(.caption).foregroundStyle(.secondary)
                    }
                    Spacer()
                    Button("Remove") { control.send(["op": "uninvite", "login": g.login]) }
                }
            }
            TextField("Their Tailscale login (name@example.com)", text: $control.guestLogin)
            Picker("For", selection: $control.guestHours) {
                Text("1 hour").tag(1)
                Text("4 hours").tag(4)
                Text("1 day").tag(24)
                Text("3 days").tag(72)
                Text("1 week").tag(168)
                Text("30 days").tag(720)
            }
            Toggle("View only (they can't control this Mac)", isOn: $control.guestViewOnly)
            HStack {
                if let m = control.guestMessage { Text(m).font(.callout).foregroundStyle(.secondary) }
                Spacer()
                Button("Invite Someone…") { control.invite() }
                    .disabled(!control.guestLogin.contains("@"))
            }
            if let inv = control.lastInvite {
                VStack(alignment: .leading, spacing: 6) {
                    Text("Invite code for \(inv.login): \(inv.code)").font(.body.monospaced().bold()).textSelection(.enabled)
                    Text("1. Share this Mac with them in Tailscale: \(inv.admin) → \"\(inv.machine)\" → ⋯ → Share…, and send them the link.")
                    Text("2. They accept it, then open \(inv.address) and enter the code under \"I have an invite code\". It works once.")
                }
                .font(.callout).textSelection(.enabled)
            }
        } header: { Text("Guests") } footer: {
            Text("Let someone else see (or use) this Mac for a while. They sign in with their own Tailscale account and a one-time code, never your password, and the panel on this Mac shows who's connected. Invites end by themselves.")
        }
    }
}

private func shortDateTime(_ iso: String) -> String {
    let f = ISO8601DateFormatter()
    f.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
    let d = f.date(from: iso) ?? ISO8601DateFormatter().date(from: iso)
    guard let d else { return iso }
    return d.formatted(date: .abbreviated, time: .shortened)
}

extension SettingsView {

    private func shortDate(_ iso: String) -> String {
        let f = ISO8601DateFormatter()
        f.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        let d = f.date(from: iso) ?? ISO8601DateFormatter().date(from: iso)
        guard let d else { return iso }
        return d.formatted(date: .abbreviated, time: .omitted)
    }
}

// MARK: - App

func runSettings(socketPath: String) -> Never {
    let app = NSApplication.shared
    app.setActivationPolicy(.regular)

    // Minimal menu so ⌘Q, ⌘W and copy / paste work.
    let main = NSMenu()
    let appItem = NSMenuItem()
    appItem.submenu = NSMenu()
    appItem.submenu?.addItem(withTitle: "Quit tailreach Settings", action: #selector(NSApplication.terminate(_:)), keyEquivalent: "q")
    let editItem = NSMenuItem()
    let edit = NSMenu(title: "Edit")
    edit.addItem(withTitle: "Cut", action: #selector(NSText.cut(_:)), keyEquivalent: "x")
    edit.addItem(withTitle: "Copy", action: #selector(NSText.copy(_:)), keyEquivalent: "c")
    edit.addItem(withTitle: "Paste", action: #selector(NSText.paste(_:)), keyEquivalent: "v")
    edit.addItem(withTitle: "Select All", action: #selector(NSText.selectAll(_:)), keyEquivalent: "a")
    editItem.submenu = edit
    main.addItem(appItem)
    main.addItem(editItem)
    app.mainMenu = main

    let control = ControlClient(path: socketPath)
    let window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 500, height: 680),
                          styleMask: [.titled, .closable, .miniaturizable], backing: .buffered, defer: false)
    window.title = "tailreach"
    window.contentView = NSHostingView(rootView: SettingsView(control: control))
    window.center()
    window.makeKeyAndOrderFront(nil)
    NotificationCenter.default.addObserver(forName: NSWindow.willCloseNotification, object: window, queue: .main) { _ in
        app.terminate(nil)
    }
    app.activate(ignoringOtherApps: true)
    app.run()
    exit(0)
}
