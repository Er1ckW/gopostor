# Proxy settings shared by the HTTP and HTTPS servers in gopostor.conf.
# Install next to it as /etc/nginx/conf.d/gopostor.proxy.inc (the .inc suffix keeps conf.d/*.conf from loading it on its own).

limit_req zone=gopostor_req burst=40 nodelay;
limit_req_status 429;

# /api/user bodies are tiny, gopostor itself refuses more than 64 KB.
client_max_body_size 64k;

proxy_pass http://gopostor;
proxy_http_version 1.1;
proxy_set_header Connection "";
proxy_set_header Host $host;
# Nginx forwards Authorization by default, set explicitly because gopostor needs the matchmaking token.
proxy_set_header Authorization $http_authorization;
# Shown as forwarded_client in gopostor's log, gopostor only trusts it from 127.0.0.1.
proxy_set_header X-Real-IP $remote_addr;
proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
proxy_set_header X-Forwarded-Proto $scheme;

proxy_redirect off;
proxy_buffering off;
proxy_connect_timeout 3s;
# Longer than backendTimeoutMs (5 s): gopostor answers itself when a backend is slow.
proxy_read_timeout 15s;
proxy_send_timeout 15s;
# Never retry matchmaking requests on another attempt, a PUT must not be sent twice.
proxy_next_upstream off;
