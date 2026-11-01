// pattern — an animated test-pattern window for benchmarking the screen stream.
//
//   swiftc -O tools/pattern/main.swift -o /tmp/tailreach-pattern && /tmp/tailreach-pattern [seconds]
//
// It puts a borderless, non-activating, always-visible window on the main display that
// redraws continuously: moving bars (so ScreenCaptureKit emits frames without pause),
// a frame counter (so dropped frames are visible in recordings) and the host clock in
// whole milliseconds, as text (so glass-to-glass latency can be measured with a camera
// pointed at a viewer showing this window next to it) and as a strip of 24 black/white
// cells along the top (so a viewer on the same Mac can read it from each frame it gets).
// Exits after `seconds` (default 60).
//
// Displaying a window needs no macOS permissions; it only draws.
import AppKit

final class PatternView: NSView {
    static var noiseBlocks = 500
    var frameCount = 0
    var startTime = CACurrentMediaTime()
    var startHostMs = 0.0

    override func draw(_ dirtyRect: NSRect) {
        frameCount += 1
        let t = CACurrentMediaTime() - startTime
        NSColor.black.setFill()
        bounds.fill()

        // Moving bars: one full-width sweep per 2 s, one vertical per 1.3 s.
        NSColor(red: 0.1, green: 0.5, blue: 1.0, alpha: 1).setFill()
        let x = CGFloat(t.truncatingRemainder(dividingBy: 2) / 2) * bounds.width
        NSRect(x: x - 8, y: 0, width: 16, height: bounds.height).fill()
        NSColor(red: 0.0, green: 0.9, blue: 0.4, alpha: 1).setFill()
        let y = CGFloat(t.truncatingRemainder(dividingBy: 1.3) / 1.3) * bounds.height
        NSRect(x: 0, y: y - 4, width: bounds.width, height: 8).fill()

        // Fresh random blocks every frame: real entropy, so the encoder actually
        // has work to do and the bitrate lands in the multiple-Mbit/s range a
        // congested-link test needs (bars alone compress to ~1 Mbit/s).
        for _ in 0..<Self.noiseBlocks {
            let bx = CGFloat(Double.random(in: 0..<1)) * (bounds.width - 6)
            let by = CGFloat(Double.random(in: 0..<1)) * (bounds.height - 6)
            let v = CGFloat(Double.random(in: 0..<1))
            NSColor(white: v, alpha: 1).setFill()
            NSRect(x: bx, y: by, width: 6, height: 6).fill()
        }

        // Host clock in ms and the frame counter, big enough to read on a phone photo.
        let ms = Int(t * 1000 + startHostMs)

        // The same clock as 24 black/white cells along the top (MSB first, ms mod 2^24), so a
        // viewer can read it back from every frame and measure latency without a camera.
        let cells = 24, cw = bounds.width / CGFloat(cells), ch = bounds.height / 12
        for i in 0..<cells {
            ((ms >> (cells - 1 - i)) & 1 == 1 ? NSColor.white : NSColor.black).setFill()
            NSRect(x: CGFloat(i) * cw, y: bounds.height - ch, width: cw, height: ch).fill()
        }
        let para = NSMutableParagraphStyle()
        para.alignment = .center
        let big = NSFont.monospacedDigitSystemFont(ofSize: bounds.height / 5, weight: .bold)
        let small = NSFont.monospacedDigitSystemFont(ofSize: bounds.height / 14, weight: .bold)
        let bigAttrs: [NSAttributedString.Key: Any] = [
            .font: big, .foregroundColor: NSColor.white, .paragraphStyle: para,
        ]
        NSAttributedString(string: String(ms), attributes: bigAttrs)
            .draw(in: NSRect(x: 0, y: bounds.height * 0.55, width: bounds.width, height: bounds.height / 4))
        NSAttributedString(string: "#\(frameCount)", attributes: [
            .font: small, .foregroundColor: NSColor.white, .paragraphStyle: para,
        ]).draw(in: NSRect(x: 0, y: bounds.height * 0.25, width: bounds.width, height: bounds.height / 6))
    }
}

final class Pattern {
    let app = NSApplication.shared
    var window: NSWindow!
    var view: PatternView!

    init(blocks: Int) {
        PatternView.noiseBlocks = blocks
    }

    func run(seconds: Double) {
        app.setActivationPolicy(.accessory)
        let full = NSScreen.main!.frame
        let side = min(full.width, full.height) * 0.75
        let rect = NSRect(x: full.midX - side / 2, y: full.midY - side / 2, width: side, height: side)
        view = PatternView(frame: NSRect(origin: .zero, size: rect.size))
        view.startHostMs = Date().timeIntervalSince1970 * 1000
        window = NSWindow(contentRect: rect, styleMask: [.borderless], backing: .buffered, defer: false)
        window.contentView = view
        window.level = .floating
        window.collectionBehavior = [.canJoinAllSpaces, .fullScreenAuxiliary]
        window.isOpaque = true
        window.ignoresMouseEvents = true
        window.orderFrontRegardless()

        // Keep the content dirty at ~120 Hz; the display refresh is the real frame clock.
        let t = Timer(timeInterval: 1.0 / 120, repeats: true) { [weak view] _ in view?.needsDisplay = true }
        RunLoop.main.add(t, forMode: .common)
        Timer.scheduledTimer(withTimeInterval: seconds, repeats: false) { _ in NSApp.terminate(nil) }
        app.run()
    }
}

// pattern [seconds] [noise-blocks]  — blocks default 500; 250 ≈ 3 Mbit/s of screen
// entropy (a busy real screen), 500 ≈ 5 Mbit/s (worst case).
let args = CommandLine.arguments
let secs = args.count > 1 ? Double(args[1]) ?? 60 : 60
let blocks = args.count > 2 ? Int(args[2]) ?? 500 : 500
Pattern(blocks: blocks).run(seconds: secs)
