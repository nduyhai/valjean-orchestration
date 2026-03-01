import argparse
import asyncio
import json
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer
from urllib import request

# ---- LiveKit Agents imports (adjust to your installed version) ----
# If your demo currently runs, reuse the same imports/classes you already have.
from livekit import rtc
from livekit.agents.voice import AgentSession


AGENT_HTTP_PORT = 7010

# Shared session reference so /cmd can call say/interrupt
SESSION: AgentSession | None = None

def post_json(url: str, payload: dict):
    data = json.dumps(payload).encode("utf-8")
    req = request.Request(url, data=data, headers={"Content-Type": "application/json"}, method="POST")
    try:
        with request.urlopen(req, timeout=3) as resp:
            resp.read()
    except Exception as e:
        print(f"[agent] failed POST {url}: {e}")

class CmdHandler(BaseHTTPRequestHandler):
    def do_POST(self):
        global SESSION

        if self.path != "/cmd":
            self.send_response(404)
            self.end_headers()
            return

        length = int(self.headers.get("Content-Length", "0"))
        body = self.rfile.read(length)
        try:
            cmd = json.loads(body.decode("utf-8"))
        except Exception:
            self.send_response(400)
            self.end_headers()
            self.wfile.write(b"bad json")
            return

        # cmd: { room, action: say|interrupt, text }
        action = cmd.get("action")
        text = cmd.get("text", "")

        if SESSION is None:
            self.send_response(503)
            self.end_headers()
            self.wfile.write(b"session not ready")
            return

        # Run async calls on the AgentSession loop
        async def handle():
            if action == "interrupt":
                await SESSION.interrupt()
            elif action == "say":
                if not text:
                    return
                # Allow interruptions by default; you can add allow_interruptions to payload later
                await SESSION.say(text, allow_interruptions=True)

        asyncio.run_coroutine_threadsafe(handle(), SESSION.loop)

        self.send_response(200)
        self.end_headers()
        self.wfile.write(b"ok")

def start_cmd_server():
    server = HTTPServer(("0.0.0.0", AGENT_HTTP_PORT), CmdHandler)
    print(f"[agent] cmd server listening on :{AGENT_HTTP_PORT}")
    server.serve_forever()

async def main():
    global SESSION

    parser = argparse.ArgumentParser()
    parser.add_argument("--room", required=True)
    parser.add_argument("--orchestrator", required=True)  # e.g. http://localhost:8080
    parser.add_argument("--livekit-url", default="ws://localhost:7880")
    parser.add_argument("--api-key", default="devkey")
    parser.add_argument("--api-secret", default="secret")
    args = parser.parse_args()

    # Create token for identity "agent"
    # For demo, easiest is to generate token via your orchestrator or a small helper.
    # Here’s a simple approach: use livekit server-sdk in Go normally.
    #
    # If your existing demo agent already connects successfully, reuse that token code.
    #
    # Placeholder: you MUST replace get_token() with your token generation.
    from livekit.api import AccessToken, VideoGrants  # works if livekit-api python installed

    token = (
        AccessToken(args.api_key, args.api_secret)
        .with_identity("agent")
        .with_name("agent")
        .with_grants(VideoGrants(room_join=True, room=args.room))
        .to_jwt()
    )

    room = rtc.Room()
    await room.connect(args.livekit_url, token)
    print(f"[agent] connected, joined room={args.room} identity=agent")

    # Create AgentSession (your voice pipeline)
    SESSION = AgentSession(room=room)
    # Expose session loop for run_coroutine_threadsafe
    SESSION.loop = asyncio.get_running_loop()

    # Start command server in background thread
    threading.Thread(target=start_cmd_server, daemon=True).start()

    # Hook: when user transcript is ready
    @SESSION.on("user_input_transcribed")
    async def on_transcript(ev):
        # ev.transcript, ev.is_final, ev.language, ev.speaker_id (depends on version)
        text = getattr(ev, "transcript", "")
        is_final = getattr(ev, "is_final", True)

        if not text:
            return

        if is_final:
            post_json(
                f"{args.orchestrator}/agent/event",
                {
                    "room": args.room,
                    "type": "transcript",
                    "transcript": text,
                },
            )

    # Start the session
    await SESSION.start()

    # Keep running until disconnected
    while True:
        await asyncio.sleep(1)

if __name__ == "__main__":
    asyncio.run(main())
