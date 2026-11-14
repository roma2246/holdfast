# Окружение для записи демо-гифки: VHS + git + jq + свежий holdfast.
# Собирается и запускается скриптом docs/demo/record.sh.
FROM ghcr.io/charmbracelet/vhs:v0.12.1

RUN apt-get update \
	&& apt-get install -y --no-install-recommends git jq ca-certificates \
	&& rm -rf /var/lib/apt/lists/*

COPY holdfast /usr/local/bin/holdfast
COPY agent.sh /usr/local/bin/agent
COPY setup.sh /usr/local/share/holdfast-demo/setup.sh
