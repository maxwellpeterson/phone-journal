# syntax=docker/dockerfile:1

# Adapted from https://docs.docker.com/language/golang/build-images/

FROM golang:1.19-buster AS build

WORKDIR /app

RUN git clone https://github.com/ggerganov/whisper.cpp.git
RUN cd whisper.cpp/bindings/go && make whisper
RUN cd /app && bash whisper.cpp/models/download-ggml-model.sh tiny.en

COPY go.mod ./
COPY go.sum ./
RUN go mod download

COPY server.go ./
ENV C_INCLUDE_PATH /app/whisper.cpp
ENV LIBRARY_PATH /app/whisper.cpp
RUN go build -o /phone-journal-server server.go

FROM debian:buster-slim

RUN apt-get update
RUN apt-get install -y ca-certificates
RUN update-ca-certificates

COPY --from=build /phone-journal-server /phone-journal-server
COPY --from=build /app/whisper.cpp/models/ggml-tiny.en.bin /ggml-tiny.en.bin

EXPOSE 80

ENTRYPOINT [ "/phone-journal-server" ]
