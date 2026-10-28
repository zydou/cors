FROM gcr.io/distroless/static:nonroot AS base

FROM base AS amd64
COPY cors-x86_64-unknown-linux-musl /cors

FROM base AS arm64
COPY cors-aarch64-unknown-linux-musl /cors

# 根据平台选择对应的二进制文件
FROM ${TARGETARCH} AS final
ENV PORT=3000
EXPOSE 3000
CMD ["/cors"]
