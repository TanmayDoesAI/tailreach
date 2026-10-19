// mydevices-capture — the native half of mydevices on macOS.
//
//   mydevices-capture                   streams the screen as low-latency H.264 while a viewer is
//                                       attached, injects input, shows the "screen shared" panel
//   mydevices-capture --settings SOCK   the settings window (see settings.swift)
//
// `mydevices` keeps one warm for a short while after a viewer leaves, so coming back is instant.
//   stdout: frames  [u32 big-endian length][u8 type][payload]
//             type 1 = decoder config: [u16 json length][json {codec,w,h}][avcC]
//             type 2 = video:          [u8 keyframe][u64 timestamp µs][AVCC NAL units]
//             type 3 = event (json):   {"t":"started"|"error"|"cursor",...}
//   stdin:   one JSON command per line (see `handle(_:)`)
import AppKit
import ApplicationServices
import SwiftUI
import CoreMedia
import Foundation
import ScreenCaptureKit
import VideoToolbox

signal(SIGPIPE, SIG_IGN)

// MARK: - Output

let outLock = NSLock()

func emit(_ type: UInt8, _ payload: Data) {
    var frame = Data(capacity: payload.count + 5)
    let n = UInt32(payload.count).bigEndian
    withUnsafeBytes(of: n) { frame.append(contentsOf: $0) }
    frame.append(type)
    frame.append(payload)
    outLock.lock()
    defer { outLock.unlock() }
    frame.withUnsafeBytes { raw in
        var off = 0
        while off < raw.count {
            let w = write(1, raw.baseAddress! + off, raw.count - off)
            if w <= 0 {
                if errno == EINTR { continue }
                exit(0) // parent went away
            }
            off += w
        }
    }
}

func event(_ obj: [String: Any]) {
    if let d = try? JSONSerialization.data(withJSONObject: obj) { emit(3, d) }
}

func log(_ s: String) {
    FileHandle.standardError.write("[capture] \(s)\n".data(using: .utf8)!)
}

// MARK: - Encoder (VideoToolbox, hardware H.264, low-latency rate control)

final class Encoder {
    private var session: VTCompressionSession?
    private var lastConfig: Data?
    var forceKey = true
    var bitrate = 8_000_000
    var fps = 60

    func setup(width: Int, height: Int) {
        invalidate()
        let spec = [kVTVideoEncoderSpecification_EnableLowLatencyRateControl: kCFBooleanTrue] as CFDictionary
        var s: VTCompressionSession?
        let st = VTCompressionSessionCreate(
            allocator: nil, width: Int32(width), height: Int32(height),
            codecType: kCMVideoCodecType_H264, encoderSpecification: spec,
            imageBufferAttributes: nil, compressedDataAllocator: nil,
            outputCallback: nil, refcon: nil, compressionSessionOut: &s)
        guard st == noErr, let s else {
            event(["t": "error", "msg": "encoder create failed (\(st))"])
            return
        }
        VTSessionSetProperty(s, key: kVTCompressionPropertyKey_RealTime, value: kCFBooleanTrue)
        VTSessionSetProperty(s, key: kVTCompressionPropertyKey_AllowFrameReordering, value: kCFBooleanFalse)
        VTSessionSetProperty(s, key: kVTCompressionPropertyKey_ProfileLevel, value: kVTProfileLevel_H264_ConstrainedHigh_AutoLevel)
        VTSessionSetProperty(s, key: kVTCompressionPropertyKey_ExpectedFrameRate, value: fps as CFNumber)
        VTSessionSetProperty(s, key: kVTCompressionPropertyKey_MaxKeyFrameIntervalDuration, value: 30 as CFNumber)
        VTSessionSetProperty(s, key: kVTCompressionPropertyKey_PrioritizeEncodingSpeedOverQuality, value: kCFBooleanTrue)
        VTCompressionSessionPrepareToEncodeFrames(s)
        session = s
        lastConfig = nil
        forceKey = true
        setBitrate(bitrate)
    }

    /// The next keyframe carries the decoder config again (for a viewer that just reconnected).
    func resendConfig() {
        lastConfig = nil
        forceKey = true
    }

    func setBitrate(_ bps: Int) {
        bitrate = max(500_000, min(bps, 50_000_000))
        guard let s = session else { return }
        VTSessionSetProperty(s, key: kVTCompressionPropertyKey_AverageBitRate, value: bitrate as CFNumber)
    }

    func encode(_ pb: CVPixelBuffer, pts: CMTime) {
        guard let s = session else { return }
        var props: CFDictionary?
        if forceKey {
            props = [kVTEncodeFrameOptionKey_ForceKeyFrame: kCFBooleanTrue] as CFDictionary
            forceKey = false
        }
        VTCompressionSessionEncodeFrame(s, imageBuffer: pb, presentationTimeStamp: pts, duration: .invalid,
                                        frameProperties: props, infoFlagsOut: nil) { [weak self] status, _, sb in
            guard status == noErr, let sb else { return }
            self?.output(sb)
        }
    }

    private func output(_ sb: CMSampleBuffer) {
        guard let block = sb.dataBuffer else { return }
        let attachments = CMSampleBufferGetSampleAttachmentsArray(sb, createIfNecessary: false) as? [[CFString: Any]]
        let isKey = !((attachments?.first?[kCMSampleAttachmentKey_NotSync] as? Bool) ?? false)

        if isKey, let fd = sb.formatDescription,
           let atoms = CMFormatDescriptionGetExtension(fd, extensionKey: kCMFormatDescriptionExtension_SampleDescriptionExtensionAtoms) as? [String: Any],
           let avcC = atoms["avcC"] as? Data, avcC.count >= 4, avcC != lastConfig {
            lastConfig = avcC
            let dims = CMVideoFormatDescriptionGetDimensions(fd)
            let codec = String(format: "avc1.%02x%02x%02x", avcC[1], avcC[2], avcC[3])
            let json = try! JSONSerialization.data(withJSONObject: ["codec": codec, "w": dims.width, "h": dims.height])
            var p = Data()
            withUnsafeBytes(of: UInt16(json.count).bigEndian) { p.append(contentsOf: $0) }
            p.append(json)
            p.append(avcC)
            emit(1, p)
        }

        let len = CMBlockBufferGetDataLength(block)
        var p = Data(count: 9 + len)
        p[0] = isKey ? 1 : 0
        let us = UInt64(max(0, CMSampleBufferGetPresentationTimeStamp(sb).seconds * 1_000_000)).bigEndian
        p.withUnsafeMutableBytes { raw in
            withUnsafeBytes(of: us) { raw.baseAddress!.advanced(by: 1).copyMemory(from: $0.baseAddress!, byteCount: 8) }
            _ = CMBlockBufferCopyDataBytes(block, atOffset: 0, dataLength: len, destination: raw.baseAddress!.advanced(by: 9))
        }
        emit(2, p)
    }

    func invalidate() {
        if let s = session {
            VTCompressionSessionCompleteFrames(s, untilPresentationTimeStamp: .invalid)
            VTCompressionSessionInvalidate(s)
        }
        session = nil
    }
}

// MARK: - Capture (ScreenCaptureKit)

final class Capture: NSObject, SCStreamOutput, SCStreamDelegate {
    let q = DispatchQueue(label: "capture", qos: .userInteractive)
    let enc = Encoder()
    private var stream: SCStream?
    private var lastPB: CVPixelBuffer?
    private var lastFrameAt = Date()
    private var refinements = 0
    private var timer: DispatchSourceTimer?
    private var params: [Int] = []
    private var startedEvent: [String: Any] = [:]

    func start(maxW: Int, maxH: Int, fps: Int, cursor: Bool) async {
        // Same settings and still running (a viewer coming back): just send a fresh keyframe.
        let p = [maxW, maxH, fps, cursor ? 1 : 0]
        if stream != nil, p == params {
            q.async { self.enc.resendConfig() }
            event(startedEvent)
            return
        }
        do {
            let content = try await SCShareableContent.excludingDesktopWindows(false, onScreenWindowsOnly: true)
            guard let d = content.displays.first(where: { $0.displayID == CGMainDisplayID() }) ?? content.displays.first else {
                event(["t": "error", "msg": "no display"])
                return
            }
            // Leave our own "screen shared" panel out of the stream.
            let filter: SCContentFilter
            if let me = content.applications.first(where: { $0.processID == getpid() }) {
                filter = SCContentFilter(display: d, excludingApplications: [me], exceptingWindows: [])
            } else {
                filter = SCContentFilter(display: d, excludingWindows: [])
            }
            let scale = Double(filter.pointPixelScale)
            let nativeW = Double(filter.contentRect.width) * scale
            let nativeH = Double(filter.contentRect.height) * scale
            let f = min(1.0, Double(maxW) / nativeW, Double(maxH) / nativeH)
            let w = Int(nativeW * f) & ~1
            let h = Int(nativeH * f) & ~1

            let cfg = SCStreamConfiguration()
            cfg.width = w
            cfg.height = h
            cfg.minimumFrameInterval = CMTime(value: 1, timescale: CMTimeScale(fps))
            cfg.pixelFormat = kCVPixelFormatType_420YpCbCr8BiPlanarVideoRange // what the encoder wants: no conversion
            cfg.colorMatrix = CGDisplayStream.yCbCrMatrix_ITU_R_709_2
            cfg.queueDepth = 5
            cfg.showsCursor = cursor // false when the viewer draws the cursor itself (feels instant)
            cfg.capturesAudio = false // video only: no sound is captured or sent

            if let old = stream { try? await old.stopCapture() }
            q.sync {
                enc.fps = fps
                enc.setup(width: w, height: h)
                lastPB = nil
            }
            let s = SCStream(filter: filter, configuration: cfg, delegate: self)
            try s.addStreamOutput(self, type: .screen, sampleHandlerQueue: q)
            try await s.startCapture()
            stream = s
            params = p
            startRefineTimer()
            startedEvent = ["t": "started", "w": w, "h": h, "nativeW": Int(nativeW), "nativeH": Int(nativeH),
                            "ptW": Double(filter.contentRect.width), "ptH": Double(filter.contentRect.height)]
            event(startedEvent)
        } catch {
            event(["t": "error", "msg": error.localizedDescription, "needsPermission": !CGPreflightScreenCaptureAccess()])
        }
    }

    func stop() async {
        timer?.cancel()
        if let s = stream { try? await s.stopCapture() }
        stream = nil
        params = []
    }

    func stream(_ stream: SCStream, didOutputSampleBuffer sb: CMSampleBuffer, of type: SCStreamOutputType) {
        guard type == .screen, sb.isValid,
              let info = (CMSampleBufferGetSampleAttachmentsArray(sb, createIfNecessary: false) as? [[SCStreamFrameInfo: Any]])?.first,
              let raw = info[.status] as? Int, SCFrameStatus(rawValue: raw) == .complete,
              let pb = sb.imageBuffer else { return }
        lastPB = pb
        lastFrameAt = Date()
        refinements = 0
        enc.encode(pb, pts: sb.presentationTimeStamp)
    }

    func stream(_ stream: SCStream, didStopWithError error: Error) {
        event(["t": "error", "msg": error.localizedDescription])
    }

    /// When the screen stops changing, re-encode the last frame a few times so text sharpens up.
    private func startRefineTimer() {
        timer?.cancel()
        let t = DispatchSource.makeTimerSource(queue: q)
        t.schedule(deadline: .now() + .milliseconds(100), repeating: .milliseconds(100))
        t.setEventHandler { [weak self] in
            guard let self, let pb = self.lastPB else { return }
            let idle = Date().timeIntervalSince(self.lastFrameAt)
            if (idle > 0.2 && self.refinements < 3) || self.enc.forceKey {
                self.refinements += 1
                self.enc.encode(pb, pts: CMClockGetTime(CMClockGetHostTimeClock()))
            }
        }
        t.resume()
        timer = t
    }

    func requestKeyframe() { q.async { self.enc.forceKey = true } }
    func resendConfig() { q.async { self.enc.resendConfig() } }
    func setBitrate(_ bps: Int) { q.async { self.enc.setBitrate(bps) } }
}

// MARK: - Input (CGEvent)

final class Input {
    let src = CGEventSource(stateID: .hidSystemState)
    var down = [false, false, false]

    var bounds: CGRect { CGDisplayBounds(CGMainDisplayID()) }
    var pos: CGPoint { CGEvent(source: nil)?.location ?? .zero }

    func clamp(_ p: CGPoint) -> CGPoint {
        let b = bounds
        return CGPoint(x: min(max(p.x, b.minX), b.maxX - 1), y: min(max(p.y, b.minY), b.maxY - 1))
    }

    func move(to p: CGPoint, mods: CGEventFlags) {
        let p = clamp(p)
        let (type, btn): (CGEventType, CGMouseButton) =
            down[0] ? (.leftMouseDragged, .left) : down[1] ? (.rightMouseDragged, .right) : (.mouseMoved, .left)
        let e = CGEvent(mouseEventSource: src, mouseType: type, mouseCursorPosition: p, mouseButton: btn)
        e?.flags = mods
        e?.post(tap: .cghidEventTap)
    }

    func moveAbs(_ x: Double, _ y: Double, mods: CGEventFlags) {
        let b = bounds
        move(to: CGPoint(x: b.minX + x * b.width, y: b.minY + y * b.height), mods: mods)
    }

    func moveRel(_ dx: Double, _ dy: Double, mods: CGEventFlags) {
        let p = pos
        move(to: CGPoint(x: p.x + dx, y: p.y + dy), mods: mods)
    }

    func button(_ b: Int, down isDown: Bool, clicks: Int, mods: CGEventFlags) {
        guard (0...2).contains(b) else { return }
        let types: [(CGEventType, CGEventType)] = [(.leftMouseDown, .leftMouseUp), (.rightMouseDown, .rightMouseUp), (.otherMouseDown, .otherMouseUp)]
        let e = CGEvent(mouseEventSource: src, mouseType: isDown ? types[b].0 : types[b].1,
                        mouseCursorPosition: pos, mouseButton: CGMouseButton(rawValue: UInt32(b))!)
        e?.setIntegerValueField(.mouseEventClickState, value: Int64(max(1, clicks)))
        e?.flags = mods
        e?.post(tap: .cghidEventTap)
        down[b] = isDown
    }

    /// Scrolls like a real trackpad: continuous pixel deltas tagged with a gesture phase
    /// (began/changed/ended) or a momentum phase (begin/continue/end), so Mac apps glide,
    /// decelerate and rubber-band natively.
    func scroll(_ dx: Double, _ dy: Double, phase: String?, momentum: String?) {
        guard let e = CGEvent(scrollWheelEvent2Source: src, units: .pixel, wheelCount: 2,
                              wheel1: Int32(dy.rounded()), wheel2: Int32(dx.rounded()), wheel3: 0) else { return }
        e.setIntegerValueField(.scrollWheelEventIsContinuous, value: 1)
        e.setDoubleValueField(.scrollWheelEventFixedPtDeltaAxis1, value: dy)
        e.setDoubleValueField(.scrollWheelEventFixedPtDeltaAxis2, value: dx)
        e.setIntegerValueField(.scrollWheelEventPointDeltaAxis1, value: Int64(dy.rounded()))
        e.setIntegerValueField(.scrollWheelEventPointDeltaAxis2, value: Int64(dx.rounded()))
        let phases: [String: Int64] = ["begin": 1, "change": 2, "end": 4, "cancel": 8]
        let momenta: [String: Int64] = ["begin": 1, "continue": 2, "end": 3]
        if let p = phase, let v = phases[p] { e.setIntegerValueField(.scrollWheelEventScrollPhase, value: v) }
        if let m = momentum, let v = momenta[m] { e.setIntegerValueField(.scrollWheelEventMomentumPhase, value: v) }
        e.post(tap: .cghidEventTap)
    }

    func key(_ name: String, mods: CGEventFlags) {
        var flags = mods
        var code: CGKeyCode
        if let c = Input.named[name] {
            code = c
            if Input.arrows.contains(name) { flags.formUnion([.maskNumericPad, .maskSecondaryFn]) }
        } else if name.count == 1, let c = Input.chars[Character(name.lowercased())] {
            code = c
            if name != name.lowercased() { flags.insert(.maskShift) }
        } else {
            type(name)
            return
        }
        for d in [true, false] {
            let e = CGEvent(keyboardEventSource: src, virtualKey: code, keyDown: d)
            e?.flags = flags
            e?.post(tap: .cghidEventTap)
        }
    }

    func type(_ text: String) {
        for ch in text {
            if ch == "\n" || ch == "\r" { key("return", mods: []); continue }
            let u = Array(String(ch).utf16)
            for d in [true, false] {
                let e = CGEvent(keyboardEventSource: src, virtualKey: 0, keyDown: d)
                e?.keyboardSetUnicodeString(stringLength: u.count, unicodeString: u)
                e?.post(tap: .cghidEventTap)
            }
        }
    }

    // Media / brightness keys go through NSEvent "system defined" events.
    func media(_ name: String) {
        let keys: [String: Int] = ["volup": 0, "voldown": 1, "brup": 2, "brdown": 3, "mute": 7, "play": 16, "next": 17, "prev": 18]
        guard let k = keys[name] else { return }
        for d in [true, false] {
            let flags = NSEvent.ModifierFlags(rawValue: d ? 0xa00 : 0xb00)
            let ev = NSEvent.otherEvent(with: .systemDefined, location: .zero, modifierFlags: flags, timestamp: 0,
                                        windowNumber: 0, context: nil, subtype: 8,
                                        data1: (k << 16) | (d ? 0xa00 : 0xb00), data2: -1)
            ev?.cgEvent?.post(tap: .cghidEventTap)
        }
    }

    static func flags(_ mods: [String]) -> CGEventFlags {
        var f: CGEventFlags = []
        for m in mods {
            switch m {
            case "cmd": f.insert(.maskCommand)
            case "opt": f.insert(.maskAlternate)
            case "ctrl": f.insert(.maskControl)
            case "shift": f.insert(.maskShift)
            case "fn": f.insert(.maskSecondaryFn)
            default: break
            }
        }
        return f
    }

    static let arrows: Set<String> = ["left", "right", "up", "down"]
    static let named: [String: CGKeyCode] = [
        "return": 36, "tab": 48, "space": 49, "backspace": 51, "esc": 53, "delete": 117,
        "left": 123, "right": 124, "down": 125, "up": 126,
        "home": 115, "end": 119, "pageup": 116, "pagedown": 121,
        "f1": 122, "f2": 120, "f3": 99, "f4": 118, "f5": 96, "f6": 97, "f7": 98, "f8": 100,
        "f9": 101, "f10": 109, "f11": 103, "f12": 111,
    ]
    static let chars: [Character: CGKeyCode] = [
        "a": 0, "s": 1, "d": 2, "f": 3, "h": 4, "g": 5, "z": 6, "x": 7, "c": 8, "v": 9, "b": 11,
        "q": 12, "w": 13, "e": 14, "r": 15, "y": 16, "t": 17, "1": 18, "2": 19, "3": 20, "4": 21,
        "6": 22, "5": 23, "=": 24, "9": 25, "7": 26, "-": 27, "8": 28, "0": 29, "]": 30, "o": 31,
        "u": 32, "[": 33, "i": 34, "p": 35, "l": 37, "j": 38, "'": 39, "k": 40, ";": 41, "\\": 42,
        ",": 43, "/": 44, "n": 45, "m": 46, ".": 47, "`": 50, " ": 49,
    ]
}

// MARK: - Commands

let capture = Capture()
let input = Input()
let inputQ = DispatchQueue(label: "input", qos: .userInteractive)

func num(_ v: Any?) -> Double { (v as? NSNumber)?.doubleValue ?? 0 }

func handle(_ c: [String: Any]) {
    let mods = Input.flags(c["m"] as? [String] ?? [])
    switch c["t"] as? String {
    case "start":
        let w = Int(num(c["w"])), h = Int(num(c["h"])), fps = Int(num(c["fps"]))
        if let br = c["br"] as? NSNumber { capture.enc.bitrate = br.intValue }
        let localCursor = c["localCursor"] as? Bool ?? false
        let viewer = c["viewer"] as? String ?? "a device"
        Task {
            await MainActor.run { panel.show(viewer: viewer) } // shown before capture starts, so it can be left out of it
            await capture.start(maxW: max(320, w), maxH: max(240, h), fps: (10...120).contains(fps) ? fps : 60, cursor: !localCursor)
        }
    case "stop":
        Task {
            await capture.stop()
            await MainActor.run { panel.hide() }
        }
    case "kf": capture.requestKeyframe()
    case "resend": capture.resendConfig()
    case "br": capture.setBitrate(Int(num(c["bps"])))
    case "abs": inputQ.async { input.moveAbs(num(c["x"]), num(c["y"]), mods: mods) }
    case "rel": inputQ.async { input.moveRel(num(c["dx"]), num(c["dy"]), mods: mods) }
    case "btn": inputQ.async { input.button(Int(num(c["b"])), down: c["down"] as? Bool ?? false, clicks: Int(num(c["n"])), mods: mods) }
    case "click":
        inputQ.async {
            let b = Int(num(c["b"])), n = max(1, Int(num(c["n"])))
            input.button(b, down: true, clicks: n, mods: mods)
            input.button(b, down: false, clicks: n, mods: mods)
        }
    case "scroll": inputQ.async { input.scroll(num(c["dx"]), num(c["dy"]), phase: c["ph"] as? String, momentum: c["mo"] as? String) }
    case "key": inputQ.async { input.key(c["k"] as? String ?? "", mods: mods) }
    case "text": inputQ.async { input.type(c["s"] as? String ?? "") }
    case "media": inputQ.async { input.media(c["k"] as? String ?? "") }
    case "clip-set": DispatchQueue.main.async { clipboard.set(c["s"] as? String ?? "") }
    case "clip-get": DispatchQueue.main.async { clipboard.send(auto: false) }
    default: break
    }
}

// MARK: - Clipboard

final class Clipboard {
    private let pb = NSPasteboard.general
    private var lastCount: Int
    private var timer: Timer?

    init() {
        lastCount = pb.changeCount
        // Tell the viewer when something new is copied on the Mac.
        timer = Timer.scheduledTimer(withTimeInterval: 0.5, repeats: true) { [weak self] _ in
            guard let self, self.pb.changeCount != self.lastCount else { return }
            self.lastCount = self.pb.changeCount
            self.send(auto: true)
        }
    }

    func set(_ s: String) {
        pb.clearContents()
        pb.setString(s, forType: .string)
        lastCount = pb.changeCount
    }

    func send(auto: Bool) {
        // Never forward secrets that password managers mark as concealed or transient.
        let types = pb.types ?? []
        if types.contains(NSPasteboard.PasteboardType("org.nspasteboard.ConcealedType"))
            || types.contains(NSPasteboard.PasteboardType("org.nspasteboard.TransientType")) { return }
        guard let s = pb.string(forType: .string), !s.isEmpty else { return }
        event(["t": "clip", "s": String(s.prefix(256 * 1024)), "auto": auto])
    }
}

// MARK: - Cursor

/// Sends the cursor's position (60 Hz, on change) and its current shape (on change),
/// so the viewer can draw it locally with no round-trip delay.
final class CursorWatcher {
    private var lastPos = CGPoint(x: -1, y: -1)
    private var lastShape = 0
    private var tick = 0
    private var timer: Timer?

    init() {
        timer = Timer.scheduledTimer(withTimeInterval: 1.0 / 60, repeats: true) { [weak self] _ in self?.poll() }
        RunLoop.main.add(timer!, forMode: .common)
    }

    private func poll() {
        let b = CGDisplayBounds(CGMainDisplayID())
        if let p = CGEvent(source: nil)?.location, p != lastPos {
            lastPos = p
            event(["t": "cursor", "x": (p.x - b.minX) / b.width, "y": (p.y - b.minY) / b.height])
        }
        tick += 1
        guard tick % 6 == 0, let cur = NSCursor.currentSystem else { return } // shape: 10×/s
        let img = cur.image
        guard let tiff = img.tiffRepresentation else { return }
        let shape = tiff.hashValue ^ Int(cur.hotSpot.x * 1000) ^ Int(cur.hotSpot.y)
        guard shape != lastShape,
              let rep = NSBitmapImageRep(data: tiff),
              let png = rep.representation(using: .png, properties: [:]) else { return }
        lastShape = shape
        event(["t": "cursorImage", "png": png.base64EncodedString(),
               "w": img.size.width, "h": img.size.height, "hx": cur.hotSpot.x, "hy": cur.hotSpot.y])
    }
}

// MARK: - Lock screen

/// Reports when the Mac locks or unlocks, so the viewer can offer to unlock it.
final class LockWatcher {
    private var last: Bool?
    private var timer: Timer?

    init() {
        timer = Timer.scheduledTimer(withTimeInterval: 0.5, repeats: true) { [weak self] _ in self?.check() }
        check()
    }

    private func check() {
        let d = CGSessionCopyCurrentDictionary() as? [String: Any]
        let locked = (d?["CGSSessionScreenIsLocked"] as? Bool) ?? false
        if locked != last {
            last = locked
            event(["t": "lock", "locked": locked])
        }
    }
}

// MARK: - Entry

let args = CommandLine.arguments
func arg(_ name: String) -> String? {
    guard let i = args.firstIndex(of: name), i + 1 < args.count else { return nil }
    return args[i + 1]
}

if let sock = arg("--settings") {
    runSettings(socketPath: sock) // never returns
}

let app = NSApplication.shared
app.setActivationPolicy(.accessory) // no Dock icon, no menu bar
let clipboard = Clipboard()
let lockWatcher = LockWatcher()
let cursorWatcher = CursorWatcher()
let panel = SharePanel {
    event(["t": "stop-request"]) // mydevices ends the session and this process
}
// Warm up ScreenCaptureKit while idle, so the first frame comes quickly. (No capture yet.)
Task { _ = try? await SCShareableContent.excludingDesktopWindows(false, onScreenWindowsOnly: true) }

// Ask for permissions up front so macOS lists this helper in System Settings.
if !CGPreflightScreenCaptureAccess() { CGRequestScreenCaptureAccess() }
if !AXIsProcessTrustedWithOptions([kAXTrustedCheckOptionPrompt.takeUnretainedValue() as String: true] as CFDictionary) {
    event(["t": "warn", "msg": "Accessibility permission is needed for mouse and keyboard control"])
}

Thread {
    while let line = readLine(strippingNewline: true) {
        guard let d = line.data(using: .utf8),
              let obj = try? JSONSerialization.jsonObject(with: d) as? [String: Any] else { continue }
        handle(obj)
    }
    exit(0) // stdin closed: session over
}.start()

app.run()
