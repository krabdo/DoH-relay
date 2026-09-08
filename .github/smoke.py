"""Exercise a real DNS GET and POST through the published container."""
import base64
import time
import urllib.request

# example.com A, recursion desired
query = bytes.fromhex("123401000001000000000000076578616d706c6503636f6d0000010001")
url = "http://127.0.0.1:8080/dns-query"
for attempt in range(30):
    try:
        request = urllib.request.Request(url + "?dns=" + base64.urlsafe_b64encode(query).decode().rstrip("="), headers={"Accept": "application/dns-message"})
        with urllib.request.urlopen(request, timeout=15) as response:
            body = response.read()
            assert response.status == 200 and len(body) >= 12 and body[:2] == query[:2] and body[2] & 128
        break
    except OSError:
        if attempt == 29:
            raise
        time.sleep(1)
with urllib.request.urlopen(urllib.request.Request(url, data=query, headers={"Content-Type": "application/dns-message", "Accept": "application/dns-message"}), timeout=15) as response:
    body = response.read()
    assert response.status == 200 and len(body) >= 12 and body[:2] == query[:2] and body[2] & 128
print("Container DNS GET and POST passed")
