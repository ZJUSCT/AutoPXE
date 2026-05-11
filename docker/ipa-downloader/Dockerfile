FROM alpine:3.21

RUN apk add --no-cache curl tar

COPY get-resource.sh /usr/local/bin/

ENTRYPOINT ["/usr/local/bin/get-resource.sh"]
