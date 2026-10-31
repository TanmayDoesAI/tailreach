// mydevices-capture — the native half of mydevices on macOS.
//
//   mydevices-capture                   streams the screen as low-latency H.264 while a viewer is
//                                       attached, injects input, shows the "screen shared" panel
//   mydevices-capture --settings SOCK   the settings window (see settings.swift)
//
// `mydevices` keeps one warm for a short while after a viewer leaves, so coming back is instant.
//   stdout: frames  [u32 big-endian length][u8 type][payload]
//             type 1 = decoder config: [u16 json length][json {codec,w,h}][avcC or hvcC]
//             type 2 = video:          [u8 keyframe][u64 timestamp µs][length-prefixed NAL units]
//             type 3 = event (json):   {"t":"started"|"error"|"cursor"|"displays",...}
//             type 4 = audio:          [u64 timestamp µs][one 20 ms Opus packet] (only if the viewer asked)
//   stdin:   one JSON command per line (see `handle(_:)`)
import AppKit
import ApplicationServices
import AVFoundation
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
    // NaN or infinity (a display that just went away) would raise an ObjC exception that
    // `try?` can't catch, and the helper would die without ending its capture session.
    guard JSONSerialization.isValidJSONObject(obj),
          let d = try? JSONSerialization.data(withJSONObject: obj) else { return }
    emit(3, d)
}

func log(_ s: String) {
    FileHandle.standardError.write("[capture] \(s)\n".data(using: .utf8)!)
}

/// Debug logging, compiled in only by `make DEBUG=1` (-D DEBUG_LOG). The message is an
/// autoclosure, so a normal build never even builds the string.
@inline(__always) func dlog(_ msg: @autoclosure () -> String) {
    #if DEBUG_LOG
    FileHandle.standardError.write("\(dlogTime.string(from: Date())) · [capture] \(msg())\n".data(using: .utf8)!)
    #endif
}
#if DEBUG_LOG
private let dlogTime: DateFormatter = { let f = DateFormatter(); f.dateFormat = "HH:mm:ss"; return f }()
#endif

// MARK: - Encoder (VideoToolbox, hardware H.264 or HEVC, low-latency rate control)

final class Encoder {
    private var session: VTCompressionSession?
    private var lastConfig: Data?
    private var boosted = false
    var forceKey = true
    var bitrate = 8_000_000
    var fps = 60
    var hevc = false // HEVC: sharper text at the same bitrate, for viewers that can decode it

    func setup(width: Int, height: Int) {
        invalidate()
        let spec = [kVTVideoEncoderSpecification_EnableLowLatencyRateControl: kCFBooleanTrue] as CFDictionary
        var s: VTCompressionSession?
        let st = VTCompressionSessionCreate(
            allocator: nil, width: Int32(width), height: Int32(height),
            codecType: hevc ? kCMVideoCodecType_HEVC : kCMVideoCodecType_H264, encoderSpecification: spec,
            imageBufferAttributes: nil, compressedDataAllocator: nil,
            outputCallback: nil, refcon: nil, compressionSessionOut: &s)
        guard st == noErr, let s else {
            event(["t": "error", "msg": "encoder create failed (\(st))"])
            return
        }
        VTSessionSetProperty(s, key: kVTCompressionPropertyKey_RealTime, value: kCFBooleanTrue)
        VTSessionSetProperty(s, key: kVTCompressionPropertyKey_AllowFrameReordering, value: kCFBooleanFalse)
        VTSessionSetProperty(s, key: kVTCompressionPropertyKey_ProfileLevel,
                             value: hevc ? kVTProfileLevel_HEVC_Main_AutoLevel : kVTProfileLevel_H264_ConstrainedHigh_AutoLevel)
        VTSessionSetProperty(s, key: kVTCompressionPropertyKey_ExpectedFrameRate, value: fps as CFNumber)
        VTSessionSetProperty(s, key: kVTCompressionPropertyKey_MaxKeyFrameIntervalDuration, value: 30 as CFNumber)
        VTSessionSetProperty(s, key: kVTCompressionPropertyKey_PrioritizeEncodingSpeedOverQuality, value: kCFBooleanTrue)
        VTCompressionSessionPrepareToEncodeFrames(s)
        session = s
        lastConfig = nil
        forceKey = true
        boosted = false
        setBitrate(bitrate)
    }

    /// The next keyframe carries the decoder config again (for a viewer that just reconnected).
    func resendConfig() {
        lastConfig = nil
        forceKey = true
    }

    func setBitrate(_ bps: Int) {
        bitrate = max(500_000, min(bps, 50_000_000))
        dlog("encoder bitrate \(bitrate / 1000) kbps")
        guard let s = session else { return }
        boosted = false
        VTSessionSetProperty(s, key: kVTCompressionPropertyKey_AverageBitRate, value: bitrate as CFNumber)
    }

    /// More bits for the few frames that sharpen a still screen: the link is idle then, and
    /// at the normal rate each of them only gets a moving frame's share.
    func boost(_ on: Bool) {
        guard on != boosted, let s = session else { return }
        boosted = on
        let bps = on ? min(bitrate * 5 / 2, 50_000_000) : bitrate
        VTSessionSetProperty(s, key: kVTCompressionPropertyKey_AverageBitRate, value: bps as CFNumber)
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
           let cfg = (atoms["avcC"] ?? atoms["hvcC"]) as? Data, cfg.count >= 13, cfg != lastConfig {
            lastConfig = cfg
            let dims = CMVideoFormatDescriptionGetDimensions(fd)
            // WebCodecs' name for the stream: profile and level from the config record.
            let codec = atoms["hvcC"] != nil
                ? String(format: "hvc1.%d.6.%@%d.B0", cfg[1] & 0x1f, cfg[1] & 0x20 != 0 ? "H" : "L", cfg[12])
                : String(format: "avc1.%02x%02x%02x", cfg[1], cfg[2], cfg[3])
            let json = try! JSONSerialization.data(withJSONObject: ["codec": codec, "w": dims.width, "h": dims.height])
            var p = Data()
            withUnsafeBytes(of: UInt16(json.count).bigEndian) { p.append(contentsOf: $0) }
            p.append(json)
            p.append(cfg)
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

// MARK: - Audio (the Mac's sound as Opus, only when the viewer turns it on)

/// Collects the captured sound into 20 ms blocks and encodes each as one Opus packet,
/// which is what WebRTC carries.
final class AudioEncoder {
    static let rate = 48_000.0
    static let block: AVAudioFrameCount = 960 // 20 ms
    private let pcm = AVAudioFormat(commonFormat: .pcmFormatFloat32, sampleRate: rate, channels: 2, interleaved: false)!
    private var opus: AVAudioFormat
    private let conv: AVAudioConverter?
    private let pending: AVAudioPCMBuffer
    private var pendingAt: UInt64 = 0 // µs of the first sample in `pending`

    init() {
        var asbd = AudioStreamBasicDescription(mSampleRate: AudioEncoder.rate, mFormatID: kAudioFormatOpus, mFormatFlags: 0,
                                               mBytesPerPacket: 0, mFramesPerPacket: UInt32(AudioEncoder.block), mBytesPerFrame: 0,
                                               mChannelsPerFrame: 2, mBitsPerChannel: 0, mReserved: 0)
        opus = AVAudioFormat(streamDescription: &asbd)!
        conv = AVAudioConverter(from: pcm, to: opus)
        conv?.bitRate = 96_000
        pending = AVAudioPCMBuffer(pcmFormat: pcm, frameCapacity: AudioEncoder.block)!
    }

    func add(_ sb: CMSampleBuffer) {
        guard let fd = sb.formatDescription, let asbd = CMAudioFormatDescriptionGetStreamBasicDescription(fd),
              let fmt = AVAudioFormat(streamDescription: asbd),
              fmt.commonFormat == .pcmFormatFloat32, !fmt.isInterleaved, fmt.sampleRate == AudioEncoder.rate else {
            dlog("audio: unexpected format \(String(describing: sb.formatDescription))")
            return
        }
        let n = AVAudioFrameCount(sb.numSamples)
        guard n > 0, let buf = AVAudioPCMBuffer(pcmFormat: fmt, frameCapacity: n) else { return }
        buf.frameLength = n // sizes the buffer list the copy fills
        let st = CMSampleBufferCopyPCMDataIntoAudioBufferList(sb, at: 0, frameCount: Int32(n), into: buf.mutableAudioBufferList)
        guard st == noErr, let src = buf.floatChannelData, let dst = pending.floatChannelData else {
            dlog("audio: copy failed (\(st)), \(fmt)")
            return
        }
        var at = UInt64(max(0, sb.presentationTimeStamp.seconds * 1_000_000))
        var off: AVAudioFrameCount = 0
        while off < n {
            if pending.frameLength == 0 { pendingAt = at }
            let take = min(n - off, AudioEncoder.block - pending.frameLength)
            for c in 0..<2 {
                let from = src[min(c, Int(fmt.channelCount) - 1)] + Int(off) // mono: the same in both ears
                (dst[c] + Int(pending.frameLength)).update(from: from, count: Int(take))
            }
            pending.frameLength += take
            off += take
            at += UInt64(Double(take) / AudioEncoder.rate * 1_000_000)
            if pending.frameLength == AudioEncoder.block {
                encode()
                pending.frameLength = 0
            }
        }
    }

    private func encode() {
        guard let conv else { return }
        let out = AVAudioCompressedBuffer(format: opus, packetCapacity: 1, maximumPacketSize: max(conv.maximumOutputPacketSize, 1500))
        var fed = false
        var err: NSError?
        _ = conv.convert(to: out, error: &err) { _, status in
            if fed { status.pointee = .noDataNow; return nil }
            fed = true
            status.pointee = .haveData
            return self.pending
        }
        guard err == nil, out.packetCount > 0, out.byteLength > 0 else {
            dlog("audio: encode gave nothing (\(String(describing: err)))")
            return
        }
        var p = Data(capacity: 8 + Int(out.byteLength))
        withUnsafeBytes(of: pendingAt.bigEndian) { p.append(contentsOf: $0) }
        p.append(Data(bytes: out.data, count: Int(out.byteLength)))
        emit(4, p)
    }
}

// MARK: - Capture (ScreenCaptureKit)

final class Capture: NSObject, SCStreamOutput, SCStreamDelegate {
    let q = DispatchQueue(label: "capture", qos: .userInteractive)
    let audioQ = DispatchQueue(label: "audio", qos: .userInteractive)
    let enc = Encoder()
    private var audio: AudioEncoder?
    private var stream: SCStream?
    private var lastPB: CVPixelBuffer?
    private var lastFrameAt = Date()
    private var refinements = 0
    private var timer: DispatchSourceTimer?
    private var params: [Int] = []
    private var startedEvent: [String: Any] = [:]
    private var wanted: (Int, Int, Int, Bool, CGDirectDisplayID, Bool, Bool)? // what the viewer asked for, for automatic retries
    private var retries = 0

    /// macOS's capture service sometimes drops a session (it's busy, or the Mac is hot).
    /// Start it again a few times, backing off, while someone is still watching.
    private func retryLater(_ why: String) {
        guard let w = wanted, retries < 5 else {
            event(["t": "error", "msg": why, "needsPermission": !CGPreflightScreenCaptureAccess()])
            return
        }
        retries += 1
        let delay = 0.3 * pow(2, Double(retries - 1))
        dlog("capture start failed (\(why)), retry \(retries) in \(delay) s")
        log("capture interrupted (\(why)); retry \(retries) in \(delay)s")
        DispatchQueue.main.asyncAfter(deadline: .now() + delay) {
            guard viewing else { return }
            Task { self.stream = nil; self.params = []; await self.start(maxW: w.0, maxH: w.1, fps: w.2, cursor: w.3, display: w.4, hevc: w.5, sound: w.6) }
        }
    }

    /// display 0 means the main display. One that's gone (unplugged) falls back to it too.
    func start(maxW: Int, maxH: Int, fps: Int, cursor: Bool, display: CGDirectDisplayID, hevc: Bool, sound: Bool) async {
        // Same settings and still running (a viewer coming back): just send a fresh keyframe.
        wanted = (maxW, maxH, fps, cursor, display, hevc, sound)
        let p = [maxW, maxH, fps, cursor ? 1 : 0, Int(display), hevc ? 1 : 0, sound ? 1 : 0]
        if stream != nil, p == params {
            dlog("start: same settings, still capturing: fresh keyframe only")
            resendConfig()
            event(startedEvent)
            return
        }
        do {
            let content = try await SCShareableContent.excludingDesktopWindows(false, onScreenWindowsOnly: true)
            guard let d = content.displays.first(where: { $0.displayID == display })
                    ?? content.displays.first(where: { $0.displayID == CGMainDisplayID() }) ?? content.displays.first else {
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
            // Sound only when the viewer turned it on (off by default). Never this helper's own.
            cfg.capturesAudio = sound
            if sound {
                cfg.sampleRate = Int(AudioEncoder.rate)
                cfg.channelCount = 2
                cfg.excludesCurrentProcessAudio = true
            }

            if let old = stream { try? await old.stopCapture() }
            q.sync {
                enc.fps = fps
                enc.hevc = hevc
                enc.setup(width: w, height: h)
                lastPB = nil
            }
            let s = SCStream(filter: filter, configuration: cfg, delegate: self)
            try s.addStreamOutput(self, type: .screen, sampleHandlerQueue: q)
            audio = sound ? AudioEncoder() : nil
            if sound { try s.addStreamOutput(self, type: .audio, sampleHandlerQueue: audioQ) }
            try await s.startCapture()
            stream = s
            params = p
            retries = 0
            shownDisplay = d.displayID // input and cursor reports follow the display being shown
            startRefineTimer()
            dlog("capturing display \(d.displayID) at \(w)x\(h) (native \(Int(nativeW))x\(Int(nativeH))), \(fps) fps, \(enc.bitrate / 1000) kbps")
            startedEvent = ["t": "started", "w": w, "h": h, "nativeW": Int(nativeW), "nativeH": Int(nativeH),
                            "ptW": Double(filter.contentRect.width), "ptH": Double(filter.contentRect.height),
                            "display": Int(d.displayID)]
            event(startedEvent)
        } catch {
            if !CGPreflightScreenCaptureAccess() {
                event(["t": "error", "msg": error.localizedDescription, "needsPermission": true])
            } else {
                retryLater(error.localizedDescription)
            }
        }
    }

    func stop() async {
        dlog("capture stopped")
        wanted = nil
        timer?.cancel()
        if let s = stream { try? await s.stopCapture() }
        stream = nil
        params = []
    }

    func stream(_ stream: SCStream, didOutputSampleBuffer sb: CMSampleBuffer, of type: SCStreamOutputType) {
        if type == .audio {
            if sb.isValid { audio?.add(sb) }
            return
        }
        guard type == .screen, sb.isValid,
              let info = (CMSampleBufferGetSampleAttachmentsArray(sb, createIfNecessary: false) as? [[SCStreamFrameInfo: Any]])?.first,
              let raw = info[.status] as? Int, SCFrameStatus(rawValue: raw) == .complete,
              let pb = sb.imageBuffer else { return }
        lastPB = pb
        lastFrameAt = Date()
        refinements = 0
        enc.boost(false)
        enc.encode(pb, pts: sb.presentationTimeStamp)
    }

    func stream(_ stream: SCStream, didStopWithError error: Error) {
        // Stop Sharing in macOS's own menu: end the session, exactly like our panel's Stop.
        if (error as NSError).domain == SCStreamErrorDomain,
           (error as NSError).code == SCStreamError.userStopped.rawValue {
            wanted = nil
            event(["t": "stop-request"])
            return
        }
        retryLater(error.localizedDescription)
    }

    /// When the screen stops changing, re-encode the last frame a few times, with more bits
    /// each (boost), so text sharpens up.
    private func startRefineTimer() {
        timer?.cancel()
        let t = DispatchSource.makeTimerSource(queue: q)
        t.schedule(deadline: .now() + .milliseconds(100), repeating: .milliseconds(100))
        t.setEventHandler { [weak self] in
            guard let self, let pb = self.lastPB else { return }
            let idle = Date().timeIntervalSince(self.lastFrameAt)
            if (idle > 0.2 && self.refinements < 4) || self.enc.forceKey {
                self.refinements += 1
                if idle > 0.2 { self.enc.boost(true) }
                self.enc.encode(pb, pts: CMClockGetTime(CMClockGetHostTimeClock()))
            }
        }
        t.resume()
        timer = t
    }

    func requestKeyframe() { dlog("keyframe requested"); q.async { self.enc.forceKey = true; self.encodeNowIfIdle() } }
    func resendConfig() { q.async { self.enc.resendConfig(); self.encodeNowIfIdle() } }

    /// A still screen produces no frames; answer a keyframe request right away from the
    /// last one instead of waiting for the refinement timer.
    private func encodeNowIfIdle() {
        guard let pb = lastPB, Date().timeIntervalSince(lastFrameAt) > 0.05 else { return }
        enc.encode(pb, pts: CMClockGetTime(CMClockGetHostTimeClock()))
    }
    func setBitrate(_ bps: Int) { q.async { self.enc.setBitrate(bps) } }
}

// MARK: - Input (CGEvent)

final class Input {
    let src = CGEventSource(stateID: .hidSystemState)
    var down = [false, false, false]
    var held = Set<CGKeyCode>() // keys pressed but not yet released

    var bounds: CGRect { CGDisplayBounds(shownDisplay) }
    var pos: CGPoint { CGEvent(source: nil)?.location ?? .zero }

    func clamp(_ p: CGPoint) -> CGPoint {
        let b = bounds
        guard b.width > 0, b.height > 0 else { return p }   // the shown display is gone
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
        guard b.width > 0, b.height > 0, x.isFinite, y.isFinite else { return }
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

    /// Lets go of any button or key still held (a viewer that left mid-drag or mid-press).
    func releaseAll() {
        for b in 0..<3 where down[b] { button(b, down: false, clicks: 1, mods: []) }
        for k in held { CGEvent(keyboardEventSource: src, virtualKey: k, keyDown: false)?.post(tap: .cghidEventTap) }
        held.removeAll()
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

    /// A key press, or with `down` just its press or its release (a key held on the viewer's
    /// keyboard: games, hold-to-repeat). `rep` marks the viewer's own key repeats.
    func key(_ name: String, mods: CGEventFlags, down: Bool? = nil, rep: Bool = false) {
        var flags = mods
        var code: CGKeyCode
        var text: [UInt16]?
        if let c = Input.named[name] {
            code = c
            if Input.arrows.contains(name) { flags.formUnion([.maskNumericPad, .maskSecondaryFn]) }
        } else if name.count == 1, let (c, shift) = Input.keyFor(Character(name)) {
            code = c
            if shift { flags.insert(.maskShift) }
            // A plain character keeps its text, so another layout on the Mac still types it.
            if flags.isDisjoint(with: [.maskCommand, .maskControl, .maskAlternate]) { text = Array(name.utf16) }
        } else {
            if down != false { type(name) }
            return
        }
        for d in down.map({ [$0] }) ?? [true, false] {
            let e = CGEvent(keyboardEventSource: src, virtualKey: code, keyDown: d)
            e?.flags = flags
            if let text { e?.keyboardSetUnicodeString(stringLength: text.count, unicodeString: text) }
            if rep { e?.setIntegerValueField(.keyboardEventAutorepeat, value: 1) }
            e?.post(tap: .cghidEventTap)
        }
        if let down { if down { held.insert(code) } else { held.remove(code) } }
    }

    /// Types text. A character on a US key goes as that real key press (with Shift if it
    /// needs it), so web apps and games see the key itself: YouTube's Shift+> or Space, say.
    /// The character itself rides along, so another keyboard layout on the Mac still types it.
    func type(_ text: String) {
        for ch in text {
            if ch == "\n" || ch == "\r" { key("return", mods: []); continue }
            let u = Array(String(ch).utf16)
            let k = Input.keyFor(ch)
            for d in [true, false] {
                let e = CGEvent(keyboardEventSource: src, virtualKey: k?.0 ?? 0, keyDown: d)
                e?.flags = k?.1 == true ? .maskShift : []
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
    /// The characters Shift makes on a US keyboard, and the key they're on.
    static let shifted: [Character: Character] = [
        "!": "1", "@": "2", "#": "3", "$": "4", "%": "5", "^": "6", "&": "7", "*": "8", "(": "9", ")": "0",
        "_": "-", "+": "=", "{": "[", "}": "]", "|": "\\", ":": ";", "\"": "'", "<": ",", ">": ".", "?": "/", "~": "`",
    ]

    /// The US key a character is on, and whether it needs Shift.
    static func keyFor(_ ch: Character) -> (CGKeyCode, Bool)? {
        if let c = chars[ch] { return (c, false) }
        if ch.isUppercase, let l = ch.lowercased().first, let c = chars[l] { return (c, true) }
        if let b = shifted[ch], let c = chars[b] { return (c, true) }
        return nil
    }
}

// MARK: - Commands

let capture = Capture()
let input = Input()
let inputQ = DispatchQueue(label: "input", qos: .userInteractive)

func num(_ v: Any?) -> Double { (v as? NSNumber)?.doubleValue ?? 0 }

/// Clicks from a viewer that points directly (touch, mouse) carry their own position, so
/// they land in the right place even if the move before them was lost or arrives late.
func moveIfGiven(_ c: [String: Any], mods: CGEventFlags) {
    if let x = c["x"] as? NSNumber, let y = c["y"] as? NSNumber { input.moveAbs(x.doubleValue, y.doubleValue, mods: mods) }
}

func handle(_ c: [String: Any]) {
    let mods = Input.flags(c["m"] as? [String] ?? [])
    switch c["t"] as? String {
    case "start":
        let w = Int(num(c["w"])), h = Int(num(c["h"])), fps = Int(num(c["fps"]))
        if let br = c["br"] as? NSNumber { capture.enc.bitrate = br.intValue }
        let localCursor = c["localCursor"] as? Bool ?? false
        let viewer = c["viewer"] as? String ?? "a device"
        let display = CGDirectDisplayID(max(0, min(num(c["display"]), Double(UInt32.max))))
        if !AXIsProcessTrusted() {
            event(["t": "warn", "msg": "Accessibility permission is needed for mouse and keyboard control"])
        }
        Task {
            inputQ.async { input.releaseAll() }
            await MainActor.run {
                viewing = true
                lockWatcher.reset()
                cursorWatcher.reset()
                panel.show(viewer: viewer) // shown before capture starts, so it can be left out of it
                sendDisplays()
            }
            await capture.start(maxW: max(320, w), maxH: max(240, h), fps: (10...120).contains(fps) ? fps : 60,
                                cursor: !localCursor, display: display, hevc: c["codec"] as? String == "hevc",
                                sound: c["audio"] as? Bool ?? false)
        }
    case "stop":
        inputQ.async { input.releaseAll() }
        Task {
            await capture.stop()
            await MainActor.run { viewing = false; panel.hide() }
        }
    case "release": inputQ.async { input.releaseAll() }
    case "kf": capture.requestKeyframe()
    case "resend": capture.resendConfig()
    case "displays": DispatchQueue.main.async { sendDisplays() }
    case "diag":
        // "ask" comes only from `mydevices doctor --ask` on this Mac (the server strips it from
        // viewers): it puts mydevices in both Privacy lists, and macOS may show its prompt.
        let ask = c["ask"] as? Bool ?? false
        DispatchQueue.main.async {
            if ask {
                _ = CGRequestScreenCaptureAccess()
                _ = AXIsProcessTrustedWithOptions([kAXTrustedCheckOptionPrompt.takeUnretainedValue() as String: true] as CFDictionary)
            }
            event(["t": "diag", "accessibility": AXIsProcessTrusted(), "screen": CGPreflightScreenCaptureAccess()])
        }
    case "br": capture.setBitrate(Int(num(c["bps"])))
    case "abs": inputQ.async { input.moveAbs(num(c["x"]), num(c["y"]), mods: mods) }
    case "rel": inputQ.async { input.moveRel(num(c["dx"]), num(c["dy"]), mods: mods) }
    case "btn": inputQ.async { moveIfGiven(c, mods: mods); input.button(Int(num(c["b"])), down: c["down"] as? Bool ?? false, clicks: Int(num(c["n"])), mods: mods) }
    case "click":
        inputQ.async {
            let b = Int(num(c["b"])), n = max(1, Int(num(c["n"])))
            moveIfGiven(c, mods: mods)
            input.button(b, down: true, clicks: n, mods: mods)
            input.button(b, down: false, clicks: n, mods: mods)
        }
    case "scroll": inputQ.async { input.scroll(num(c["dx"]), num(c["dy"]), phase: c["ph"] as? String, momentum: c["mo"] as? String) }
    case "key": inputQ.async { input.key(c["k"] as? String ?? "", mods: mods, down: c["down"] as? Bool, rep: c["rep"] as? Bool ?? false) }
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
            guard viewing, let self, self.pb.changeCount != self.lastCount else { return }
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

/// True while a viewer is attached and capture is running. Watchers idle otherwise.
var viewing = false

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

    /// Send position and shape again (a viewer just started).
    func reset() { lastPos = CGPoint(x: -1, y: -1); lastShape = 0; tick = 14 }

    private func poll() {
        guard viewing else { return }
        let b = CGDisplayBounds(shownDisplay)
        guard b.width > 0, b.height > 0 else { return }   // the shown display is gone: wait for the next start
        if let p = CGEvent(source: nil)?.location, p != lastPos {
            lastPos = p
            event(["t": "cursor", "x": (p.x - b.minX) / b.width, "y": (p.y - b.minY) / b.height])
        }
        tick += 1
        guard tick % 15 == 0, let cur = NSCursor.currentSystem else { return } // shape: 4×/s
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

// MARK: - Displays

/// The display being shown. Input positions and cursor reports are relative to it.
var shownDisplay = CGMainDisplayID()

/// Tells the viewer which displays this Mac has, so it can offer a choice. Main thread.
func sendDisplays() {
    let key = NSDeviceDescriptionKey("NSScreenNumber")
    let list: [[String: Any]] = NSScreen.screens.compactMap { s in
        guard let id = (s.deviceDescription[key] as? NSNumber)?.uint32Value else { return nil }
        return ["id": Int(id), "name": s.localizedName, "main": CGDisplayIsMain(id) != 0,
                "builtin": CGDisplayIsBuiltin(id) != 0, "w": Int(s.frame.width), "h": Int(s.frame.height)]
    }
    event(["t": "displays", "list": list])
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

    /// Report the current state again (a viewer just started).
    func reset() { last = nil; check() }

    private func check() {
        guard viewing else { return }
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

// Only mydevices may start the capture side. Run any other way (by hand from a terminal),
// macOS would credit the screen recording to that app (Terminal, Warp, …) and show it as
// "sharing" with no Stop that works, so refuse unless our own parent launched us.
if ProcessInfo.processInfo.environment["MYDEVICES_PARENT"] != String(getppid()) {
    FileHandle.standardError.write("mydevices-capture is started by mydevices; don't run it directly.\n".data(using: .utf8)!)
    exit(2)
}

let app = NSApplication.shared
app.setActivationPolicy(.accessory) // no Dock icon, no menu bar
let clipboard = Clipboard()
let lockWatcher = LockWatcher()
let cursorWatcher = CursorWatcher()
let panel = SharePanel {
    event(["t": "stop-request"]) // mydevices ends the session and this process
}
// A display plugged in or removed: let the viewer update its list.
NotificationCenter.default.addObserver(forName: NSApplication.didChangeScreenParametersNotification, object: nil, queue: .main) { _ in
    if viewing { sendDisplays() }
}
// Warm up ScreenCaptureKit while idle, so the first frame comes quickly. (No capture yet.)
Task { _ = try? await SCShareableContent.excludingDesktopWindows(false, onScreenWindowsOnly: true) }

// No permission prompts from here: macOS attributes them to mydevices and asks on its
// own when something is actually missing. Just report it so the viewer can say so.
if !AXIsProcessTrusted() {
    event(["t": "warn", "msg": "Accessibility permission is needed for mouse and keyboard control"])
}

Thread {
    while let line = readLine(strippingNewline: true) {
        guard let d = line.data(using: .utf8),
              let obj = try? JSONSerialization.jsonObject(with: d) as? [String: Any] else { continue }
        handle(obj)
    }
    // stdin closed: session over (or mydevices quit). End the capture session properly
    // before exiting, so macOS clears its screen-recording indicator.
    let done = DispatchSemaphore(value: 0)
    Task { await capture.stop(); done.signal() }
    _ = done.wait(timeout: .now() + 1.5)
    log("exited cleanly")
    exit(0)
}.start()

app.run()
