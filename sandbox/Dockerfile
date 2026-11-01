# Sandbox where every agent shell command runs. ./workspace is mounted at /workspace.
FROM node:22-bookworm-slim

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      python3 python3-pip python3-venv python3-pytest python3-requests \
      git curl ca-certificates openssh-client procps jq \
 && rm -rf /var/lib/apt/lists/* \
 && ln -sf /usr/bin/python3 /usr/local/bin/python \
 && git config --system user.name "AI IT Department" \
 && git config --system user.email "ai-dept@localhost" \
 && git config --system --add safe.directory '*'

# Let pip install packages without a venv inside the throwaway sandbox.
ENV PIP_BREAK_SYSTEM_PACKAGES=1 PYTHONDONTWRITEBYTECODE=1

WORKDIR /workspace
CMD ["sleep", "infinity"]
