import asyncio
import os
import signal
import websockets

HOST = '0.0.0.0'
PORT = int(os.getenv('PORT', '8080'))
PATH = os.getenv('WS_PATH', '/ws')

async def echo(websocket):
    peer = websocket.remote_address
    print(f"[open] {peer}")
    
    # Background task to send periodic pings to keep connection alive
    async def keepalive():
        try:
            while True:
                await asyncio.sleep(30)  # Send ping every 30 seconds
                if websocket.open:
                    await websocket.ping()
                    print(f"[ping] {peer}")
        except Exception as e:
            print(f"[keepalive error] {peer}: {e}")
    
    # Start keepalive task
    keepalive_task = asyncio.create_task(keepalive())
    
    try:
        async for message in websocket:
            await websocket.send(f"echo: {message}")
    except Exception as e:
        print(f"[error] {peer}: {e}")
    finally:
        keepalive_task.cancel()
        try:
            await keepalive_task
        except asyncio.CancelledError:
            pass
        print(f"[close] {peer}")

async def main():
    async def process_request(path, request_headers):
        if path == PATH:
            return None
        if path == '/health':
            return (200, [("Content-Type", "text/plain")], b"healthy\n")
        if path == '/':
            body = b"WS echo server. Connect to /ws.\n"
            return (200, [("Content-Type", "text/plain")], body)
        return (404, [("Content-Type", "text/plain")], b"not found\n")

    async with websockets.serve(
        echo, 
        HOST, 
        PORT, 
        process_request=process_request,
        ping_interval=30,  # Send ping every 30 seconds
        ping_timeout=60,   # Wait up to 60 seconds for pong response
        close_timeout=None  # Disable close timeout
    ):
        print(f"listening on {HOST}:{PORT}, path={PATH}")
        stop = asyncio.Future()
        for sig in (signal.SIGINT, signal.SIGTERM):
            asyncio.get_event_loop().add_signal_handler(sig, stop.set_result, None)
        await stop

if __name__ == '__main__':
    asyncio.run(main())
