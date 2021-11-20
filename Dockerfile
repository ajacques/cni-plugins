FROM golang:1.17.3-bullseye AS build

WORKDIR /app

COPY go.mod ./
COPY go.sum ./
RUN go mod download

COPY . .

RUN ./build_linux.sh
RUN chmod +x /app/bin/*

# Stage that assembles all

FROM scratch AS build2

WORKDIR /work/cni

COPY --from=build /app/bin/ /work/cni/

ADD entrypoint.sh /work
ADD bridge-cni.tmpl /work


##
## Deploy
##

#FROM gcr.io/distroless/base-debian10

#WORKDIR /

#COPY --from=build /app/bin /app

FROM debian:bullseye

RUN apt-get -y update && apt-get install --no-install-recommends -qy iproute2 && apt-get clean && rm -rf /va/rlib/apt/lists/
COPY --from=build2 /work /

CMD ["/bin/sh", "/entrypoint.sh"]
