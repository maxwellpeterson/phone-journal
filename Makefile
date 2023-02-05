image:
	docker build --tag phone-journal-server .

tunnel:
	cloudflared tunnel --url http://localhost:8080

start:
	docker run -it --rm -p 8080:80 --name=phone-journal-server \
		--env MODEL_FILE=ggml-tiny.en.bin --env-file=server.env \
		phone-journal-server:latest

manifest:
	kubectl apply -f manifest.yml -n phone-journal

secret:
	kubectl apply -k . -n phone-journal

.PHONY: image tunnel start manifest secret