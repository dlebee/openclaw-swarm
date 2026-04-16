FROM ubuntu:24.04

ARG NODE_MAJOR=22
ARG OPENCLAW_VERSION=latest

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get install -y --no-install-recommends \
      openssh-server sudo curl ca-certificates gnupg iproute2 python3 git \
    && rm -rf /var/lib/apt/lists/*

RUN curl -fsSL https://deb.nodesource.com/setup_${NODE_MAJOR}.x | bash - \
    && apt-get install -y nodejs \
    && rm -rf /var/lib/apt/lists/*

RUN npm install -g openclaw@${OPENCLAW_VERSION} --quiet

# Pre-install headscale binary (mesh phase)
ARG HEADSCALE_VERSION=0.28.0
RUN ARCH=$(uname -m) \
    && case "$ARCH" in x86_64) HS_ARCH=amd64 ;; aarch64) HS_ARCH=arm64 ;; *) echo "unsupported: $ARCH" >&2; exit 1 ;; esac \
    && curl -fsSL "https://github.com/juanfont/headscale/releases/download/v${HEADSCALE_VERSION}/headscale_${HEADSCALE_VERSION}_linux_${HS_ARCH}" \
       -o /usr/local/bin/headscale \
    && chmod +x /usr/local/bin/headscale \
    && mkdir -p /var/lib/headscale /var/run/headscale /etc/headscale /var/lib/claws/headscale

# Pre-install tailscale (mesh phase)
RUN curl -fsSL https://tailscale.com/install.sh | sh

RUN useradd -m -s /bin/bash agent \
    && echo 'agent ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/agent \
    && chmod 440 /etc/sudoers.d/agent \
    && passwd -d agent

RUN mkdir -p /root/.ssh /home/agent/.ssh \
    && chmod 700 /root/.ssh /home/agent/.ssh \
    && chown -R agent:agent /home/agent/.ssh

RUN mkdir -p /var/run/sshd \
    && ssh-keygen -A \
    && sed -i 's/#\?PermitRootLogin.*/PermitRootLogin prohibit-password/' /etc/ssh/sshd_config \
    && sed -i 's/#\?PubkeyAuthentication.*/PubkeyAuthentication yes/'     /etc/ssh/sshd_config \
    && sed -i 's/#\?PasswordAuthentication.*/PasswordAuthentication no/'  /etc/ssh/sshd_config \
    && sed -i 's/#\?UsePAM.*/UsePAM no/'                                  /etc/ssh/sshd_config \
    && sed -i 's/#\?UseDNS.*/UseDNS no/'                                  /etc/ssh/sshd_config

COPY stub-systemctl.sh /usr/local/bin/systemctl
RUN chmod 755 /usr/local/bin/systemctl

COPY entrypoint.sh /entrypoint.sh
RUN chmod 755 /entrypoint.sh

EXPOSE 22
ENTRYPOINT ["/entrypoint.sh"]
