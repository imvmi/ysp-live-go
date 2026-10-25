// Local fixture derived from bkPlayURLs and fetchAbsolutePlaylist's consumed
// response fields. It does not emulate upstream authentication or CDN expiry.
const http = require("http");
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
    res.writeHead(200, {"Content-Type":"application/json"});
    return res.end(JSON.stringify({iretcode:0,playurl:"https://bklive-test.ysp.cctv.cn/live.m3u8"}));
  }
  if (req.method === "GET" && u.pathname === "/live.m3u8") {
    res.writeHead(200, {"Content-Type":"application/vnd.apple.mpegurl"});
    return res.end("#EXTM3U\n#EXT-X-TARGETDURATION:6\n#EXT-X-MEDIA-SEQUENCE:42\n#EXTINF:6,\nfresh.ts\n");
  }
  res.writeHead(501); res.end("Unmodeled fixture route");
}).listen(process.env.PORT || 4500);
