"""
valjean-agent  —  LiveKit Agents v1.0 style
============================================================
Reuses the new AgentServer / AgentSession / Agent API.

Changes from old VoiceAssistant pattern:
  - Agent class replaces VoiceAssistant
  - AgentSession wires STT/LLM/TTS/VAD/turn_detection
  - AgentServer replaces WorkerOptions
  - Commands from orchestrator still arrive via Flask :7010/cmd
  - Events (transcript, state) still POST to orchestrator /agent/event
"""

import asyncio
import logging
import os
import threading

import httpx
from dotenv import load_dotenv
from flask import Flask, request, jsonify

from livekit import rtc
from livekit.agents import (
    Agent,
    AgentServer,
    AgentSession,
    JobContext,
    JobProcess,
    cli,
    inference,
    room_io,
)
from livekit.plugins import noise_cancellation, silero
from livekit.plugins.turn_detector.multilingual import MultilingualModel

load_dotenv(".env.local")

logger = logging.getLogger("valjean.agent")
logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s [agent] %(levelname)s %(message)s",
    datefmt="%H:%M:%S",
)

# ── Config from env ───────────────────────────────────────────────────────────
ORCHESTRATOR_URL = os.getenv("ORCHESTRATOR_URL", "http://localhost:8080")
CMD_PORT         = int(os.getenv("CMD_PORT", "7010"))

# ── Shared state (session lives here so Flask thread can reach it) ────────────
_active_session: AgentSession | None = None
_active_loop:    asyncio.AbstractEventLoop | None = None
_active_room:    str = ""

# ── Sync HTTP client for fire-and-forget events to orchestrator ───────────────
_http = httpx.Client(timeout=5.0)


def post_event(type_: str, **kwargs):
    payload = {"room": _active_room, "type": type_, **kwargs}
    try:
        _http.post(f"{ORCHESTRATOR_URL}/agent/event", json=payload)
    except Exception as e:
        logger.warning(f"[event] failed to post: {e}")


# ── Agent ─────────────────────────────────────────────────────────────────────

class ValjeanAssistant(Agent):
    """
    Valjean voice assistant.
    Barge-in is handled automatically by AgentSession (MultilingualModel + Silero VAD).
    We only hook into lifecycle events to report state back to the orchestrator.
    """

    def __init__(self) -> None:
        super().__init__(
            instructions=(
                "You are Valjean, a helpful voice assistant on a phone call. "
                "Be concise — keep responses under 2 sentences unless asked for detail. "
                "No markdown, no emojis, no asterisks. Plain spoken language only."
            ),
        )

    # ── Session lifecycle ─────────────────────────────────────────────────────

    async def on_enter(self):
        """Called when agent enters the session — greet the caller."""
        logger.info("[agent] on_enter — greeting caller")
        post_event("state_change", state="listening")
        await self.session.generate_reply(
            instructions="Greet the caller warmly and ask how you can help."
        )

    async def on_exit(self):
        logger.info("[agent] on_exit")
        post_event("state_change", state="stopped")

    # ── Turn hooks ────────────────────────────────────────────────────────────

    async def on_user_turn_completed(self, turn_ctx, new_message):
        """STT has finalised the caller's utterance — report transcript."""
        transcript = ""
        if new_message and new_message.content:
            if isinstance(new_message.content, str):
                transcript = new_message.content
            else:
                transcript = " ".join(
                    p.text for p in new_message.content if hasattr(p, "text")
                )

        logger.info(f"[transcript] {transcript!r}")
        post_event("transcript", transcript=transcript)
        post_event("state_change", state="thinking")

        await super().on_user_turn_completed(turn_ctx, new_message)

    async def on_agent_turn_started(self, turn_ctx):
        post_event("state_change", state="speaking")
        await super().on_agent_turn_started(turn_ctx)

    async def on_agent_turn_completed(self, turn_ctx, new_message):
        post_event("state_change", state="listening")
        await super().on_agent_turn_completed(turn_ctx, new_message)

    # ── Barge-in ──────────────────────────────────────────────────────────────

    async def on_user_interrupted(self):
        """
        Fired by the SDK when caller speaks while agent is talking.
        SDK has already stopped TTS — we just report the state change.
        """
        logger.info("[agent] *** BARGE-IN — caller interrupted agent ***")
        post_event("state_change", state="listening")
        await super().on_user_interrupted()


# ── AgentServer setup ─────────────────────────────────────────────────────────

server = AgentServer()


def prewarm(proc: JobProcess):
    """Load VAD model once per worker process — reused across all sessions."""
    proc.userdata["vad"] = silero.VAD.load()


server.setup_fnc = prewarm


@server.rtc_session(agent_name="valjean-agent")
async def valjean_session(ctx: JobContext):
    global _active_session, _active_loop, _active_room

    ctx.log_context_fields = {"room": ctx.room.name}
    _active_room = ctx.room.name
    _active_loop = asyncio.get_running_loop()

    logger.info(f"[session] starting room={ctx.room.name}")
    post_event("state_change", state="starting")

    session = AgentSession(
        stt=inference.STT(model="deepgram/nova-3", language="multi"),
        llm=inference.LLM(model="openai/gpt-4.1-mini"),
        tts=inference.TTS(
            model="cartesia/sonic-3",
            voice="9626c31c-bec5-4cca-baa8-f8ba9e84c8bc",
        ),
        turn_detection=MultilingualModel(),
        vad=ctx.proc.userdata["vad"],
        preemptive_generation=True,
    )

    _active_session = session

    await session.start(
        agent=ValjeanAssistant(),
        room=ctx.room,
        room_options=room_io.RoomOptions(
            audio_input=room_io.AudioInputOptions(
                # Telephony noise cancellation for SIP callers, standard BVC for WebRTC
                noise_cancellation=lambda params: (
                    noise_cancellation.BVCTelephony()
                    if params.participant.kind
                       == rtc.ParticipantKind.PARTICIPANT_KIND_SIP
                    else noise_cancellation.BVC()
                ),
            ),
        ),
    )

    await ctx.connect()
    logger.info(f"[session] connected room={ctx.room.name}")


# ── Command dispatch (async, runs on agent loop) ──────────────────────────────

async def _dispatch_command(session: AgentSession, cmd: dict):
    action = cmd.get("action")

    if action == "say":
        # Orchestrator-driven speech — inject as an LLM instruction
        await session.generate_reply(
            instructions=f"Say exactly this to the user: {cmd.get('text', '')}"
        )

    elif action == "interrupt":
        session.interrupt()
        logger.info("[cmd] session interrupted")

    elif action == "transfer":
        target = cmd.get("target", "")
        logger.info(f"[cmd] transfer to {target}")
        await session.generate_reply(
            instructions="Tell the user you are transferring them now and to please hold."
        )
        # TODO: call LiveKit SIP transfer API

    elif action == "stop":
        logger.info("[cmd] stop — interrupting session")
        session.interrupt()
        # Room disconnect is handled by the job lifecycle / orchestrator

    else:
        logger.warning(f"[cmd] unknown action: {action}")


# ── Flask command server ──────────────────────────────────────────────────────

def build_cmd_server() -> Flask:
    app = Flask(__name__)
    logging.getLogger("werkzeug").setLevel(logging.ERROR)

    @app.post("/cmd")
    def cmd_endpoint():
        data = request.get_json(force=True)
        if not data:
            return jsonify({"error": "no json"}), 400

        if _active_session is None or _active_loop is None:
            return jsonify({"error": "no active session"}), 503

        logger.info(f"[cmd] action={data.get('action')}")
        future = asyncio.run_coroutine_threadsafe(
            _dispatch_command(_active_session, data),
            _active_loop,
        )
        try:
            future.result(timeout=10)
        except Exception as e:
            logger.error(f"[cmd] dispatch error: {e}")
            return jsonify({"error": str(e)}), 500

        return jsonify({"ok": True})

    @app.get("/health")
    def health():
        return jsonify({
            "room":  _active_room,
            "state": "active" if _active_session else "idle",
        })

    return app


def start_cmd_server():
    app = build_cmd_server()
    t = threading.Thread(
        target=lambda: app.run(
            host="0.0.0.0",
            port=CMD_PORT,
            debug=False,
            use_reloader=False,
        ),
        daemon=True,
        name="cmd-server",
    )
    t.start()
    logger.info(f"[cmd-server] listening on :{CMD_PORT}")


# ── Entry point ───────────────────────────────────────────────────────────────

if __name__ == "__main__":
    start_cmd_server()
    cli.run_app(server)
