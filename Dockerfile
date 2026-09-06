FROM alpine:latest

RUN apk --no-cache add ca-certificates tzdata

WORKDIR /app

# The binary can be mounted or pre-copied
COPY bin/rdp-relay-linux-amd64 /usr/local/bin/rdp-relay
RUN chmod +x /usr/local/bin/rdp-relay

VOLUME ["/etc/rdp-relay", "/var/lib/rdp-relay"]

EXPOSE 443/tcp 443/udp 9090/tcp 20000-39999/tcp 20000-39999/udp

ENTRYPOINT ["/usr/local/bin/rdp-relay"]
CMD ["--config", "/etc/rdp-relay/config.yaml"]
