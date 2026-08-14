FROM golang:alpine AS builder
WORKDIR /app
COPY go.mod go.sum main.go install.sh .
RUN ./install.sh

FROM scratch
COPY --from=builder /root/.local/bin/Strife /usr/local/bin/Strife

WORKDIR /srv/www

CMD ["Strife"]
