# The relay, as one static binary in a scratch image.
#
# Nothing is fetched at build time beyond the toolchain: this program has no
# dependencies outside the standard library, and the dashboard is compiled in.
# So the build works offline and produces the same bytes anyone else's does.

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY . .
# Static, stripped, and with the build path removed so the binary does not
# carry the directory it happened to be built in.
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/subnsub-monitor-relay .
# A state directory the unprivileged user can actually write. A VOLUME on a
# scratch image is created root-owned, and the failure that produces is silent
# in the worst way: the relay runs, the dashboard works, and the state file
# turns out to be missing the day something restarts.
RUN mkdir -p /state && chown 65534:65534 /state

FROM scratch
# The relay speaks plain HTTP behind your own TLS proxy and makes no outbound
# connections, so it needs no certificate bundle and no shell. What is in this
# image is the program.
COPY --from=build /out/subnsub-monitor-relay /subnsub-monitor-relay
COPY --from=build --chown=65534:65534 /state /var/lib/monitor-relay

# Not root. The relay opens one port and writes one file; neither needs it.
USER 65534:65534

# Inside the container, bind all interfaces — the isolation boundary is the
# container's, and Docker's own port mapping decides what is reachable. Publish
# it with `-p 127.0.0.1:8788:8788` unless a TLS proxy is in front.
EXPOSE 8788
VOLUME ["/var/lib/monitor-relay"]

ENTRYPOINT ["/subnsub-monitor-relay"]
CMD ["-listen", "0.0.0.0:8788", "-state", "/var/lib/monitor-relay/state.json"]
