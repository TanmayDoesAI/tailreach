// `tailreach-capture --menubar SOCK` — tailreach's menu-bar item. tailreach starts it and
// keeps it running; it talks only to the local control socket (owner-only, like the settings
// window), never the network. It ends when its stdin closes (tailreach stopped or hid it).
import AppKit

struct MenuSession: Decodable {
    let kind: String
    let viewer: String
    let guest: String?
}

struct MenuInvite: Decodable {
    let login: String
    let viewOnly: Bool
}

/// The status reply, all optional: while signing in to Tailscale it's only {starting, authURL}.
struct MenuState: Decodable {
    var starting: Bool?
    var authURL: String?
    var url: String? // older tailreach: just this device's address
    var viewerURL: String?
    var dashboardURL: String?
    var sessions: [MenuSession]?
    var invites: [MenuInvite]?
    var managesLogin: Bool?
    var paused: Bool?
    var keepAwake: Bool?
    var awakeOnBattery: Bool?
    var batteryFloor: Int?
    var power: PowerState?
    var update: UpdateInfo?
}

final class MenuBar: NSObject, NSMenuDelegate {
    let sock: String
    let item = NSStatusBar.system.statusItem(withLength: NSStatusItem.squareLength)
    let menu = NSMenu()
    var state: MenuState?
    var isOpen = false
    var timer: Timer?
    var settings: Process?

    init(sock: String) {
        self.sock = sock
        super.init()
        menu.delegate = self
        menu.autoenablesItems = false
        item.menu = menu
        rebuild()
        refresh()
        schedule()
    }

    // MARK: polling

    /// Every 2 s while the menu is open; otherwise every 5 s, just to keep the icon right.
    /// One short request on a local socket: no measurable CPU.
    func schedule() {
        timer?.invalidate()
        let t = Timer(timeInterval: isOpen ? 2 : 5, repeats: true) { [weak self] _ in self?.refresh() }
        t.tolerance = isOpen ? 0.2 : 1
        RunLoop.main.add(t, forMode: .common) // .common: menus run the loop in tracking mode
        timer = t
    }

    func refresh() {
        send(["op": "status", "brief": true])
    }

    /// Sends one op; the reply (always the full state) updates the menu.
    func send(_ req: [String: Any]) {
        let sock = self.sock
        DispatchQueue.global(qos: .utility).async {
            let reply = controlRoundTrip(sock, req)
            let st = reply.flatMap { try? JSONDecoder().decode(MenuState.self, from: $0) }
            DispatchQueue.main.async {
                self.state = st
                self.rebuild()
            }
        }
    }

    func menuWillOpen(_ menu: NSMenu) {
        isOpen = true
        refresh()
        schedule()
    }

    func menuDidClose(_ menu: NSMenu) {
        isOpen = false
        schedule()
    }

    // MARK: menu

    var streaming: MenuSession? { state?.sessions?.first { $0.kind == "stream" } ?? state?.sessions?.first }

    func rebuild() {
        updateIcon()
        menu.removeAllItems()
        guard let st = state else {
            menu.addItem(note("tailreach isn't responding"))
            addFooter()
            return
        }
        if st.starting == true {
            menu.addItem(note("Signing in to Tailscale…"))
            if let url = st.authURL {
                menu.addItem(action("Open Sign-In Link") { openURL(url) })
            }
            addFooter()
            return
        }

        // Who's here, or that it's ready.
        if st.paused == true {
            menu.addItem(note("Sharing paused"))
        } else if let s = streaming {
            if let g = s.guest {
                let viewOnly = st.invites?.first { $0.login.lowercased() == g.lowercased() }?.viewOnly ?? false
                menu.addItem(note("Guest: \(g)\(viewOnly ? " (view only)" : "")"))
            } else {
                menu.addItem(note("Viewing: \(s.viewer)"))
            }
        } else {
            let host = (st.dashboardURL ?? st.url).flatMap { URL(string: $0)?.host } ?? ""
            menu.addItem(note(host.isEmpty ? "Ready" : "Ready — \(host)"))
        }
        menu.addItem(.separator())

        if let url = st.viewerURL ?? st.url {
            menu.addItem(action("Copy Link") {
                NSPasteboard.general.clearContents()
                NSPasteboard.general.setString(url, forType: .string)
            })
        }
        menu.addItem(.separator())

        let pause = action("Pause Sharing") { [weak self] in
            self?.send(["op": "pause", "paused": !(st.paused ?? false)])
        }
        pause.state = st.paused == true ? .on : .off
        pause.toolTip = "No one can view this Mac until you resume, even after a restart."
        menu.addItem(pause)
        if !(st.sessions ?? []).isEmpty {
            menu.addItem(action("Stop Current Session") { [weak self] in self?.send(["op": "stopStream"]) })
        }
        menu.addItem(reachableItem(st))

        if st.managesLogin == true {
            let n = st.invites?.count ?? 0
            menu.addItem(.separator())
            menu.addItem(note(n == 0 ? "No guests" : n == 1 ? "1 guest invited" : "\(n) guests invited"))
            menu.addItem(action("Manage Guests…") { [weak self] in self?.openSettings() })
        }

        if let u = st.update {
            menu.addItem(.separator())
            menu.addItem(action("Update Available: \(u.version)") { openURL(u.url) })
        }
        addFooter()
    }

    /// Keep Mac reachable ▸ While plugged in / Also on battery, and what it can't do.
    func reachableItem(_ st: MenuState) -> NSMenuItem {
        let sub = NSMenu()
        sub.autoenablesItems = false
        let on = st.keepAwake ?? false
        let ac = action("While Plugged In") { [weak self] in self?.send(["op": "keepAwake", "enabled": !on]) }
        ac.state = on ? .on : .off
        sub.addItem(ac)
        let floor = st.batteryFloor ?? 20
        let batt = action("Also on Battery (above \(floor)%)") { [weak self] in
            self?.send(["op": "awakeOnBattery", "enabled": !(st.awakeOnBattery ?? false)])
        }
        batt.state = st.awakeOnBattery == true ? .on : .off
        batt.isEnabled = on
        sub.addItem(batt)
        sub.addItem(.separator())
        if let p = st.power {
            sub.addItem(note(p.source == "battery" ? "On battery now (\(p.percent ?? 0)%)" : "On power now"))
        }
        sub.addItem(note("The display can still sleep."))
        sub.addItem(note("With the lid closed on battery, the Mac sleeps anyway."))
        let item = NSMenuItem(title: "Keep Mac Reachable", action: nil, keyEquivalent: "")
        item.submenu = sub
        return item
    }

    func addFooter() {
        menu.addItem(.separator())
        menu.addItem(action("Settings…") { [weak self] in self?.openSettings() })
        menu.addItem(action("Check Setup…") { runDoctor() })
        menu.addItem(.separator())
        menu.addItem(action("Hide Menu-Bar Icon") { [weak self] in self?.hide() })
    }

    // MARK: icon

    /// A template symbol: plain when ready, filled while someone views, slashed when paused.
    func updateIcon() {
        var name = "rectangle.on.rectangle"
        var tip = "tailreach"
        if state?.paused == true {
            name = "rectangle.on.rectangle.slash"
            tip = "tailreach: sharing paused"
        } else if let s = streaming {
            name = "rectangle.fill.on.rectangle.fill"
            tip = "tailreach: \(s.guest ?? s.viewer) is viewing"
        }
        guard let button = item.button else { return }
        let img = NSImage(systemSymbolName: name, accessibilityDescription: tip)
            ?? NSImage(systemSymbolName: "display", accessibilityDescription: tip)
        img?.isTemplate = true
        button.image = img
        button.toolTip = tip
        button.appearsDisabled = state == nil || state?.starting == true
    }

    // MARK: actions

    func openSettings() {
        if let p = settings, p.isRunning, let app = NSRunningApplication(processIdentifier: p.processIdentifier) {
            app.activate()
            return
        }
        let p = Process()
        p.executableURL = URL(fileURLWithPath: CommandLine.arguments[0]).resolvingSymlinksInPath()
        p.arguments = ["--settings", sock]
        try? p.run()
        settings = p
    }

    func hide() {
        let alert = NSAlert()
        alert.messageText = "Hide the tailreach menu-bar icon?"
        alert.informativeText = "tailreach keeps running. To bring the icon back, open tailreach settings (run `tailreach settings`) and turn on \"Show tailreach in the menu bar\"."
        alert.addButton(withTitle: "Hide")
        alert.addButton(withTitle: "Cancel")
        NSApp.activate(ignoringOtherApps: true)
        guard alert.runModal() == .alertFirstButtonReturn else { return }
        // tailreach saves it and closes our stdin, which ends this process.
        send(["op": "menuBar", "enabled": false])
    }

    func note(_ title: String) -> NSMenuItem {
        let i = NSMenuItem(title: title, action: nil, keyEquivalent: "")
        i.isEnabled = false
        return i
    }

    func action(_ title: String, _ run: @escaping () -> Void) -> NSMenuItem {
        let i = ClosureItem(title: title, run: run)
        i.isEnabled = true
        return i
    }
}

/// A menu item that runs a closure.
final class ClosureItem: NSMenuItem {
    let run: () -> Void

    init(title: String, run: @escaping () -> Void) {
        self.run = run
        super.init(title: title, action: #selector(fire), keyEquivalent: "")
        target = self
    }

    required init(coder: NSCoder) { fatalError() }

    @objc func fire() { run() }
}

private func openURL(_ url: String) {
    if let u = URL(string: url), u.scheme == "https" { NSWorkspace.shared.open(u) }
}

/// Opens Terminal running `tailreach doctor` (a .command file: no Automation permission).
private func runDoctor() {
    let me = URL(fileURLWithPath: CommandLine.arguments[0]).resolvingSymlinksInPath()
    let tailreach = me.deletingLastPathComponent().appendingPathComponent("tailreach").path
    let file = URL(fileURLWithPath: NSTemporaryDirectory()).appendingPathComponent("tailreach-doctor.command")
    let quoted = "'" + tailreach.replacingOccurrences(of: "'", with: "'\\''") + "'"
    let script = "#!/bin/sh\nclear\n\(quoted) doctor\n"
    guard (try? script.write(to: file, atomically: true, encoding: .utf8)) != nil,
          chmod(file.path, 0o700) == 0 else { return }
    NSWorkspace.shared.open(file)
}

func runMenuBar(socketPath: String) -> Never {
    // No window server (an SSH session, a headless Mac): tailreach stops trying.
    if CGSessionCopyCurrentDictionary() == nil { exit(3) }
    // Never two: whoever holds the lock is the menu bar.
    let lock = open(socketPath + ".menubar.lock", O_CREAT | O_RDWR, 0o600)
    if lock < 0 || flock(lock, LOCK_EX | LOCK_NB) != 0 { exit(4) }

    let app = NSApplication.shared
    app.setActivationPolicy(.accessory) // no Dock icon
    let bar = MenuBar(sock: socketPath)
    // tailreach closes our stdin to stop us, and it closes by itself if tailreach dies.
    Thread {
        while readLine() != nil {}
        exit(0)
    }.start()
    withExtendedLifetime(bar) { app.run() } // the status item lives as long as bar does
    exit(0)
}
