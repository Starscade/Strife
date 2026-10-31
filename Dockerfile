FROM golang:alpine AS builder

ARG NAME
ARG INSTALL_PATH

RUN apk add --no-cache git make upx

WORKDIR /foo

COPY . .

RUN make INSTALL_PATH=${INSTALL_PATH} NAME=${NAME} && upx -9 --lzma ${INSTALL_PATH}

FROM scratch AS export

ARG NAME
ARG INSTALL_PATH

COPY --from=builder ${INSTALL_PATH} /${NAME}
