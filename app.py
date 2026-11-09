import gzip
import os
import shutil
import stat
import subprocess
import threading
import time
import gradio as gr

try:
    import spaces
    gpu_decorator = spaces.GPU
except ImportError:
    def gpu_decorator(fn):
        return fn

BOT_BINARY = "./sonar-bot-linux"
BOT_ARCHIVE = "./sonar-bot-linux.gz"
bot_process = None
bot_logs = []

def prepare_binary():
    if os.path.exists(BOT_ARCHIVE):
        with gzip.open(BOT_ARCHIVE, "rb") as f_in:
            with open(BOT_BINARY, "wb") as f_out:
                shutil.copyfileobj(f_in, f_out)
    if os.path.exists(BOT_BINARY):
        st = os.stat(BOT_BINARY)
        os.chmod(BOT_BINARY, st.st_mode | stat.S_IEXEC)
        return True
    return False

def setup_ytdlp_hooks():
    hook_dir = "/tmp/sonar_pyhook"
    os.makedirs(hook_dir, exist_ok=True)
    sitecustomize = os.path.join(hook_dir, "sitecustomize.py")
    with open(sitecustomize, "w") as f:
        f.write('''try:
    import yt_dlp
    import yt_dlp.utils._jsruntime as jsr
    jsr.NodeJsRuntime.MIN_SUPPORTED_VERSION = (14, 0, 0)

    _orig_init = yt_dlp.YoutubeDL.__init__
    _orig_extract = yt_dlp.YoutubeDL.extract_info

    def _patched_init(self, params=None, auto_init=True):
        if params is None:
            params = {}
        else:
            params = dict(params)
        cookiefile = params.pop("cookiefile", None)
        params["js_runtimes"] = {"node": {}}
        params["remote_components"] = ["ejs:github"]
        params["http_chunk_size"] = 10 * 1024 * 1024
        params["concurrent_fragment_downloads"] = 16
        if not params.get("_sonar_cookie_fallback"):
            params["extractor_args"] = {"youtube": {"player_client": ["visionos"]}}
        else:
            params.pop("_sonar_cookie_fallback", None)
            if cookiefile:
                params["cookiefile"] = cookiefile
            params["extractor_args"] = {"youtube": {"player_client": ["tv_samsung"]}}
        fmt = str(params.get("format", ""))
        if "m4a" in fmt:
            params["format"] = "140/ba[abr>=120][ext=m4a]/ba[ext=m4a]/ba"
        _orig_init(self, params, auto_init)
        self._sonar_cookiefile = cookiefile

    def _patched_extract(self, url, download=True, *args, **kwargs):
        try:
            return _orig_extract(self, url, download=download, *args, **kwargs)
        except Exception:
            cookiefile = getattr(self, "_sonar_cookiefile", None)
            if cookiefile:
                self._download_retcode = 0
                fallback_params = dict(self.params)
                fallback_params["cookiefile"] = cookiefile
                fallback_params["_sonar_cookie_fallback"] = True
                with yt_dlp.YoutubeDL(fallback_params) as ydl2:
                    return _orig_extract(ydl2, url, download=download, *args, **kwargs)
            raise

    yt_dlp.YoutubeDL.__init__ = _patched_init
    yt_dlp.YoutubeDL.extract_info = _patched_extract
except Exception as e:
    import sys
    sys.stderr.write(f"sitecustomize warning: {e}\\n")
''')
    cfg_dir = os.path.expanduser("~/.config/yt-dlp")
    os.makedirs(cfg_dir, exist_ok=True)
    with open(os.path.join(cfg_dir, "config"), "w") as f:
        f.write("--js-runtimes node\n--remote-components ejs:github\n--http-chunk-size 10M\n")
    return hook_dir

def run_bot():
    global bot_process, bot_logs
    if not prepare_binary():
        bot_logs.append("ERROR: sonar-bot-linux.gz not found!")
        return

    hook_dir = setup_ytdlp_hooks()
    env = os.environ.copy()
    existing_py = env.get("PYTHONPATH", "")
    env["PYTHONPATH"] = f"{hook_dir}:{existing_py}" if existing_py else hook_dir
    # Use port 8080 internally for the Go bot while Gradio serves port 7860
    env["PORT"] = "8080"
    # Use Long Polling on Gradio Spaces so Telegram updates and Paystack watcher work out of the box
    env["PUBLIC_URL"] = ""

    while True:
        bot_logs.append("Starting Sonar Go Engine on 2 vCPU...")
        bot_process = subprocess.Popen(
            [BOT_BINARY],
            env=env,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            text=True,
            bufsize=1,
        )
        for line in bot_process.stdout:
            bot_logs.append(line.rstrip())
            if len(bot_logs) > 100:
                bot_logs = bot_logs[-100:]
            print(line, end="", flush=True)
        bot_process.wait()
        bot_logs.append(f"Bot exited with code {bot_process.returncode}, restarting in 3s...")
        time.sleep(3)

threading.Thread(target=run_bot, daemon=True).start()

def get_status():
    running = bot_process is not None and bot_process.poll() is None
    status = "🟢 Sonar Bot Engine is ACTIVE (8 vCPU · ZeroGPU Host)" if running else "🟡 Starting..."
    recent = "\n".join(bot_logs[-25:])
    return f"### {status}\n\n```text\n{recent}\n```"

@gpu_decorator
def gpu_ping():
    return get_status()

with gr.Blocks(title="Sonar AI Status") as demo:
    gr.Markdown("# 🎧 Sonar AI Telegram Bot Engine")
    status_box = gr.Markdown(get_status)
    refresh_btn = gr.Button("Refresh Logs")
    refresh_btn.click(fn=get_status, outputs=status_box)
    gpu_btn = gr.Button("Diagnostics Ping", visible=False)
    gpu_btn.click(fn=gpu_ping, outputs=status_box)

demo.launch(server_name="0.0.0.0", server_port=7860)
