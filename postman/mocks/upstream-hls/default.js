// Local consumer-contract fixture; no real authentication or media bytes.
const http = require("http");
const playlist = (sequence, name) => `#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXT-X-MEDIA-SEQUENCE:${sequence}\n#EXTINF:6,\n${name}.ts\n`;
http.createServer(async (req, res) => {
  const u = new URL(req.url, "http://fixture.local");
  if (req.method === "POST" && u.pathname === "/control") {
    await pm.state.set("upstream-hls:mode", u.searchParams.get("mode"));
    res.writeHead(200); return res.end("ok");
  }
  const mode = await pm.state.get("upstream-hls:mode");
  if (mode === "error") {
    res.writeHead(503, {"Content-Type":"text/html"});
    return res.end("<html>Upstream temporarily unavailable</html>");
  }
  if (req.method === "GET" && u.pathname === "/") {
    let first = "live.m3u8", backups = [];
    if (mode === "slow-primary") { first = "slow.m3u8"; backups = ["healthy.m3u8"]; }
    if (mode === "frozen-primary") { first = "frozen.m3u8"; backups = ["healthy.m3u8"]; }
    if (mode === "quality") { first = "master.m3u8"; }
    const base = "https://bklive-test.ysp.cctv.cn/";
    res.writeHead(200, {"Content-Type":"application/json"});
    return res.end(JSON.stringify({iretcode:0,playurl:base+first,backurl:backups.map(path=>base+path)}));
  }
  if (req.method === "GET" && u.pathname === "/slow.m3u8") {
    // Hold the response until the caller cancels; no long-lived timer is needed.
    return;
  }
  if (req.method === "GET" && u.pathname === "/master.m3u8") {
    res.writeHead(200, {"Content-Type":"application/vnd.apple.mpegurl"});
    return res.end('#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=20000000\nhigh.m3u8\n#EXT-X-STREAM-INF:BANDWIDTH=1000000\nlow.m3u8\n');
  }
  const media = {"/live.m3u8":[42,"fresh"], "/frozen.m3u8":[42,"frozen"], "/healthy.m3u8":[43,"healthy"], "/high.m3u8":[42,"high"], "/low.m3u8":[42,"low"]};
  if (req.method === "GET" && media[u.pathname]) {
    res.writeHead(200, {"Content-Type":"application/vnd.apple.mpegurl"});
    return res.end(playlist(...media[u.pathname]));
  }
  res.writeHead(501); res.end("Unmodeled fixture route");
}).listen(process.env.PORT || 4500);
