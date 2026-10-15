# Test-only web server for the live-view checks. NOT part of the product. Plays the part camui plays in Stage 1B:
#   GET  /                serves -PageFile (same origin as everything else here)
#   POST /whep/<path>     forwards the offer to <WhepBase>/<path>/whep and adds the login server-side (-Login right|none|wrong);
#                         the browser never sees the secret. The secret is read from -SecretFile on every request, so no
#                         secret is ever on this process's command line.
#   POST /stats?run=ID    appends the body to <DataDir>\ID.jsonl
#   GET  /health
# Asynchronous (an on-demand WHEP request can wait seconds for the camera), so a slow forward never blocks the page's stats.
param(
    [Parameter(Mandatory = $true)][string]$Prefix,
    [Parameter(Mandatory = $true)][string]$PageFile,
    [Parameter(Mandatory = $true)][string]$DataDir,
    [string]$WhepBase = '',
    [string]$SecretFile = '',
    [ValidateSet('right', 'none', 'wrong')][string]$Login = 'right'
)
$ErrorActionPreference = 'Stop'
Add-Type -ReferencedAssemblies System.Net.Http -TypeDefinition @'
using System;
using System.IO;
using System.Net;
using System.Text;
public class CamLiveTestServer {
    HttpListener l = new HttpListener();
    string page, data, whep, secret, login;
    public CamLiveTestServer(string prefix, string pageFile, string dataDir, string whepBase, string secretFile, string loginMode) {
        l.Prefixes.Add(prefix); page = pageFile; data = dataDir; whep = whepBase; secret = secretFile; login = loginMode;
    }
    public void Start() { l.Start(); l.BeginGetContext(OnCtx, null); }
    void OnCtx(IAsyncResult ar) {
        HttpListenerContext c = null;
        try { c = l.EndGetContext(ar); } catch { return; }
        try { l.BeginGetContext(OnCtx, null); } catch { }
        try { Handle(c); } catch { try { c.Response.Abort(); } catch { } }
    }
    static void Send(HttpListenerResponse r, int code, string type, byte[] body) {
        r.StatusCode = code; if (type != null) r.ContentType = type;
        r.Headers["Cache-Control"] = "no-store";
        r.ContentLength64 = body.Length; r.OutputStream.Write(body, 0, body.Length); r.Close();
    }
    void Handle(HttpListenerContext c) {
        var req = c.Request; var res = c.Response; string p = req.Url.AbsolutePath;
        if (req.HttpMethod == "POST" && p == "/stats") {
            string run = Uri.UnescapeDataString((req.Url.Query ?? "").Replace("?run=", ""));
            var sb = new StringBuilder(); foreach (char ch in run) sb.Append(char.IsLetterOrDigit(ch) || ch == '-' || ch == '.' ? ch : '_');
            string body = new StreamReader(req.InputStream, Encoding.UTF8).ReadToEnd();
            lock (this) { File.AppendAllText(Path.Combine(data, sb.ToString() + ".jsonl"), body + "\n"); }
            Send(res, 204, null, new byte[0]); return;
        }
        if (req.HttpMethod == "POST" && p.StartsWith("/whep/")) {
            string name = p.Substring(6);
            foreach (char ch in name) if (!(char.IsDigit(ch) || (ch >= 'a' && ch <= 'z'))) { Send(res, 404, "text/plain", Encoding.ASCII.GetBytes("no such path")); return; }
            string sdp = new StreamReader(req.InputStream, Encoding.UTF8).ReadToEnd();
            var w = (HttpWebRequest)WebRequest.Create(whep + "/" + name + "/whep");
            w.Method = "POST"; w.ContentType = "application/sdp"; w.Proxy = null; w.Timeout = 25000;
            if (login != "none") {
                string pw = login == "wrong" ? new string('0', 64) : File.ReadAllText(secret).Trim();
                w.Headers["Authorization"] = "Basic " + Convert.ToBase64String(Encoding.UTF8.GetBytes("camlive:" + pw));
            }
            byte[] b = Encoding.UTF8.GetBytes(sdp);
            using (var s = w.GetRequestStream()) s.Write(b, 0, b.Length);
            HttpWebResponse r;
            try { r = (HttpWebResponse)w.GetResponse(); } catch (WebException e) { r = e.Response as HttpWebResponse; if (r == null) { Send(res, 502, "text/plain", Encoding.ASCII.GetBytes("upstream: " + e.Status)); return; } }
            string text = new StreamReader(r.GetResponseStream(), Encoding.UTF8).ReadToEnd();
            Send(res, (int)r.StatusCode, "application/sdp", Encoding.UTF8.GetBytes(text)); r.Close(); return;
        }
        if (p == "/health") { Send(res, 200, "text/plain", Encoding.ASCII.GetBytes("ok")); return; }
        if (req.HttpMethod == "GET" && (p == "/" || p == "/index.html")) {
            Send(res, 200, "text/html; charset=utf-8", File.ReadAllBytes(page)); return;
        }
        Send(res, 404, "text/plain", Encoding.ASCII.GetBytes("not found"));
    }
}
'@
$server = New-Object CamLiveTestServer($Prefix, $PageFile, $DataDir, $WhepBase, $SecretFile, $Login)
$server.Start()
while ($true) { Start-Sleep -Seconds 1 }
