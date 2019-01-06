# build
FROM            golang:1.11.4 as builder
ENV             GO111MODULE=on
COPY            . /go/src/github.com/doovemax/sshportal
WORKDIR         /go/src/github.com/doovemax/sshportal
RUN             make _docker_install

# minimal runtime
FROM            alpine
COPY            --from=builder /go/bin/sshportal /bin/sshportal
ENTRYPOINT      ["/bin/sshportal"]
CMD             ["server"]
EXPOSE          2222
HEALTHCHECK     CMD /bin/sshportal healthcheck --wait
