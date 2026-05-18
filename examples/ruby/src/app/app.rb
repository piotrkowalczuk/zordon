# Minimal stdlib-only HTTP server for examples/ruby — no gems, so it
# runs offline with a no-op build.
require "socket"

# Ruby block-buffers stdout when it's a pipe (which it is under alpha),
# so without sync=true the startup "up ..." line never reaches the log
# until the buffer fills or the process exits.
STDOUT.sync = true
STDERR.sync = true

addr = ARGV[1] || "127.0.0.1:8080"
host, port = addr.split(":")
server = TCPServer.new(host, port.to_i)
puts "up #{addr}"
loop do
  conn = server.accept
  conn.gets
  body = "ruby-example ok\n"
  conn.print "HTTP/1.1 200 OK\r\nContent-Length: #{body.bytesize}\r\nConnection: close\r\n\r\n#{body}"
  conn.close
end
