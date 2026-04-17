FROM gcr.io/distroless/static-debian12:nonroot AS base
ENV PORT=3000

FROM base AS amd64
COPY cors-linux-amd64 /cors

FROM base AS arm64
COPY cors-linux-arm64 /cors

FROM base AS arm
COPY cors-linux-arm /cors

FROM base AS ppc64le
COPY cors-linux-ppc64le /cors

FROM base AS s390x
COPY cors-linux-s390x /cors

# 根据平台选择对应的二进制文件
FROM ${TARGETARCH} AS final
CMD ["/cors"]
