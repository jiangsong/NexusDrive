// The arithmetic behind the transfers page's overall bar, kept apart from
// the DOM so it can be tested: what a batch reading means for the bar, and
// the rate over a short window of readings.

// batchProgress reads one UploadBatch from the status document. state is
// 'idle' (nothing to show), 'active' or 'done'. percent is by bytes when the
// batch has any, else by files — a batch of directory creations and
// deletes has no bytes and still has progress. eta is seconds, or null.
export function batchProgress(b, rate) {
  const filesTotal = b.files_total || 0;
  const filesDone = b.files_done || 0;
  const bytesTotal = b.bytes_total || 0;
  const bytesDone = b.bytes_done || 0;
  if (!b.active && !filesTotal) return { state: 'idle', percent: 0, filesTotal, filesDone, bytesTotal, bytesDone, rate: 0, eta: null };
  const state = b.active ? 'active' : 'done';
  let percent = 100;
  if (state === 'active') {
    percent = bytesTotal > 0 ? (bytesDone / bytesTotal) * 100 : filesTotal > 0 ? (filesDone / filesTotal) * 100 : 0;
    percent = Math.max(0, Math.min(99.5, percent));
  }
  let eta = null;
  if (state === 'active' && rate > 0 && bytesTotal > bytesDone) eta = (bytesTotal - bytesDone) / rate;
  return { state, percent, filesTotal, filesDone, bytesTotal, bytesDone, rate: state === 'active' ? rate : 0, eta };
}

// rateWindow keeps the last few (time, bytesDone) readings and answers the
// bytes per second across them. A window of ten seconds smooths the
// one-second ticks without lagging a minute behind; a reading from before
// the current batch — bytes went down, or the batch is not active — starts
// the window over.
export function rateWindow(windowMs = 10000) {
  let samples = [];
  return {
    sample(now, bytesDone, active) {
      if (!active || (samples.length && bytesDone < samples[samples.length - 1].b)) samples = [];
      if (!active) return 0;
      samples.push({ t: now, b: bytesDone });
      while (samples.length > 1 && now - samples[0].t > windowMs) samples.shift();
      const first = samples[0];
      const last = samples[samples.length - 1];
      const dt = (last.t - first.t) / 1000;
      if (dt <= 0) return 0;
      return Math.max(0, (last.b - first.b) / dt);
    },
  };
}
