# Minimal SSH bastion for tunnel tests: password + key auth, TCP forwarding on.
FROM alpine:3.24
RUN apk add --no-cache openssh && ssh-keygen -A \
 && adduser -D -s /bin/sh tunnel \
 && sed -i -e 's/^#\?AllowTcpForwarding.*/AllowTcpForwarding yes/' \
           -e 's/^#\?PasswordAuthentication.*/PasswordAuthentication yes/' \
           -e 's/^#\?GatewayPorts.*/GatewayPorts no/' /etc/ssh/sshd_config
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh
EXPOSE 22
ENTRYPOINT ["/entrypoint.sh"]
