FROM alpine:3.21

RUN apk add --no-cache git make gcc musl-dev perl xz-dev ccache

COPY build-ipxe.sh /bin/
COPY embed.ipxe /bin/

WORKDIR /tmp

ENTRYPOINT ["/bin/build-ipxe.sh"]
