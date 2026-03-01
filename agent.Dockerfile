# ── Python agent — uv-based ───────────────────────────────────────────────────
FROM python:3.12-slim

# Install uv from official image
COPY --from=ghcr.io/astral-sh/uv:latest /uv /usr/local/bin/uv

WORKDIR /app

# Copy dependency files first for layer cache
COPY pyproject.toml uv.lock ./

# Install deps (no venv — system Python in container is fine)
RUN uv sync --frozen --no-dev --system

# Copy source
COPY agent.py .
COPY .env.local* ./

EXPOSE 7010

ENTRYPOINT ["python", "agent.py"]