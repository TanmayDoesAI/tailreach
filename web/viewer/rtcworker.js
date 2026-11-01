// tailreach — direct video (see "direct video" in app.js). Runs as an RTCRtpScriptTransform:
// each video frame, as soon as WebRTC has all of it, goes to the page for WebCodecs to
// decode and draw, without waiting in the browser's jitter buffer. The frame also carries
// on to the browser's own decoder, whose loss recovery (keyframe requests) we keep.
'use strict';

self.onrtctransform = (e) => {
  const { readable, writable } = e.transformer;
  readable.pipeThrough(new TransformStream({
    transform(frame, out) {
      const data = frame.data.slice(0);
      self.postMessage({ data, key: frame.type === 'key', ts: frame.timestamp }, [data]);
      out.enqueue(frame);
    },
  })).pipeTo(writable).catch(() => {});
};

self.postMessage({ ready: true });   // the page waits for this: Chrome drops a transform event that comes sooner
