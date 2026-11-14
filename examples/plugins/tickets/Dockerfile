# Freeze the server into /mayfly-plugin/server, so the plugin image runs on
# any base. Mayfly never runs the image entrypoint, so the final stage can be
# FROM scratch.
FROM python:3.12-slim-bookworm AS build
WORKDIR /src
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt pyinstaller
COPY server.py .
RUN pyinstaller --onedir --name server server.py

FROM scratch
COPY --from=build /src/dist/server /mayfly-plugin/server
