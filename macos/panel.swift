// The small "Screen shared" panel shown at the top right of the Mac while a viewer is
// connected. It's left out of the stream, so only the person at the Mac sees it.
import AppKit
import Combine
import SwiftUI

final class SharePanel {
    private let panel: NSPanel
    private let model = PanelClock()

    init(onStop: @escaping () -> Void) {
        let size = NSSize(width: 300, height: 64)
        let host = FirstClickHostingView(rootView: SharePanelView(clock: model, onStop: onStop))
        host.frame = NSRect(origin: .zero, size: size)

        panel = NSPanel(contentRect: host.frame, styleMask: [.nonactivatingPanel, .borderless],
                        backing: .buffered, defer: false)
        panel.isOpaque = false
        panel.backgroundColor = .clear
        panel.hasShadow = true
        panel.level = .statusBar
        panel.collectionBehavior = [.canJoinAllSpaces, .stationary, .fullScreenAuxiliary, .ignoresCycle]
        panel.isMovableByWindowBackground = true // drag it anywhere
        panel.hidesOnDeactivate = false
        panel.contentView = host

    }

    /// Shown while the screen is being captured; hidden as soon as it stops. A guest
    /// (someone the owner invited) is named by their Tailscale login.
    func show(viewer: String, guest: String = "", viewOnly: Bool = false) {
        if !panel.isVisible {
            model.started = Date()
            // On a physical screen, not the virtual one: it's excluded from capture, so on a
            // virtual main display nobody could see it or press Stop.
            let physical = NSScreen.screens.first { s in
                (s.deviceDescription[NSDeviceDescriptionKey("NSScreenNumber")] as? CGDirectDisplayID).map(CGDisplayIsBuiltin) == 1
            }
            if let f = (physical ?? NSScreen.main)?.visibleFrame {
                panel.setFrameOrigin(NSPoint(x: f.maxX - panel.frame.width - 12, y: f.maxY - panel.frame.height - 12))
            }
        }
        model.viewer = viewer
        model.guest = guest
        model.viewOnly = viewOnly
        panel.orderFrontRegardless()
    }

    func hide() { panel.orderOut(nil) }
}

/// Lets the Stop button work on the first click without activating anything.
final class FirstClickHostingView<V: View>: NSHostingView<V> {
    override func acceptsFirstMouse(for event: NSEvent?) -> Bool { true }
}

/// Ticks once a second for the elapsed-time label. (A model object rather than @State,
/// which needs SwiftUI macro plugins that only ship with full Xcode.)
final class PanelClock: ObservableObject {
    @Published var now = Date()
    @Published var viewer = ""
    @Published var guest = "" // a guest's login; "" for the owner
    @Published var viewOnly = false
    @Published var started = Date()
    @Published var pulse = false
    private var timer: Timer?

    init() {
        timer = Timer.scheduledTimer(withTimeInterval: 1, repeats: true) { [weak self] _ in self?.now = Date() }
        DispatchQueue.main.async { self.pulse = true }
    }
}

struct SharePanelView: View {
    @ObservedObject var clock: PanelClock
    let onStop: () -> Void

    private var viewer: String { clock.guest.isEmpty ? clock.viewer : clock.guest }
    private var title: String {
        if clock.guest.isEmpty { return "Screen shared" }
        return clock.viewOnly ? "Guest · view only" : "Guest · can control"
    }
    private var started: Date { clock.started }
    private var now: Date { clock.now }
    private var pulse: Bool { clock.pulse }

    var body: some View {
        HStack(spacing: 10) {
            Circle()
                .fill(clock.guest.isEmpty ? Color.red : Color.orange)
                .frame(width: 10, height: 10)
                .opacity(pulse ? 0.35 : 1)
                .animation(.easeInOut(duration: 0.9).repeatForever(autoreverses: true), value: pulse)
            VStack(alignment: .leading, spacing: 1) {
                Text(title).font(.system(size: 13, weight: .semibold))
                Text("\(viewer) · \(elapsed)")
                    .font(.system(size: 11)).foregroundStyle(.secondary).lineLimit(1).truncationMode(.middle)
            }
            Spacer(minLength: 8)
            Button(action: onStop) {
                Text("Stop")
                    .font(.system(size: 12, weight: .semibold))
                    .foregroundStyle(.white)
                    .padding(.horizontal, 14).padding(.vertical, 6)
                    .background(Color.red, in: Capsule())
            }
            .buttonStyle(.plain)
            .help("End this remote session")
        }
        .padding(.horizontal, 14)
        .frame(width: 300, height: 64)
        .background(.regularMaterial, in: RoundedRectangle(cornerRadius: 16, style: .continuous))
        .overlay(RoundedRectangle(cornerRadius: 16, style: .continuous).strokeBorder(.white.opacity(0.08)))
    }

    private var elapsed: String {
        let s = max(0, Int(now.timeIntervalSince(started)))
        return s >= 3600 ? String(format: "%d:%02d:%02d", s / 3600, s / 60 % 60, s % 60) : String(format: "%d:%02d", s / 60, s % 60)
    }
}
