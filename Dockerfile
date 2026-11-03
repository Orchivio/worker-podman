FROM gcr.io/distroless/static-debian12:nonroot
COPY build/orchivio-worker /orchivio-worker
ENTRYPOINT ["/orchivio-worker"]
CMD ["proxy"]
