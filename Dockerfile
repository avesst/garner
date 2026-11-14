# garner container image: the static binary plus the tools rpm support
# needs (createrepo_c, rpmsign, gpg; design sections 9 and 10).

FROM docker.io/library/golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=devel
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /garner ./cmd/garner

FROM docker.io/library/almalinux:10-minimal
RUN microdnf install -y --nodocs --setopt=install_weak_deps=0 createrepo_c rpm-sign gnupg2 \
    && microdnf clean all \
    && useradd --system --home-dir /var/lib/garner --create-home garner
COPY --from=build /garner /usr/local/bin/garner
USER garner
VOLUME /var/lib/garner
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/garner"]
CMD ["serve", "--config", "/etc/garner/garner.yaml"]
