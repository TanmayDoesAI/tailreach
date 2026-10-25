"""Switch phone stats recording on for N seconds (0 stops it), over the local control socket.

    python3 tools/phonetest/record.py 900

While on, the viewer sends its WebRTC receive stats each second and the Mac logs both
sides to ~/Library/Application Support/mydevices/viewer-stats.jsonl (see vrec.go).
"""
import socket,json,sys,os
s=socket.socket(socket.AF_UNIX); s.connect(os.path.expanduser('~/Library/Application Support/mydevices/control.sock'))
s.sendall((json.dumps({'op':'recordViewer','secs':int(sys.argv[1])})+'\n').encode())
print(s.makefile().readline().strip())
