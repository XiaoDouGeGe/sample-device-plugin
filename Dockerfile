FROM alpine:latest

COPY ./build/cola /root/cola

CMD ["/root/cola"]
