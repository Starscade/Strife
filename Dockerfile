FROM golang:alpine AS builder

RUN apk add --no-cache git make

WORKDIR /app

COPY . .

RUN make


FROM scratch

COPY --from=builder /root/.local/bin/Strife /usr/local/bin/Strife
WORKDIR /srv/www

ENTRYPOINT ["Strife"]
