// echo is a conformance-test fixture: a Java service that mirrors back
// its execution context (env, cwd, argv, the JVM running it) as JSON so
// tests can assert on what zordon actually passed it. It is the JVM twin
// of golden/{go,rust,nodejs}/echo and speaks the identical wire shape, so
// the Go-side harness decodes all of them with one struct.
//
// JDK-only on purpose (com.sun.net.httpserver): no third-party deps, so
// the Maven/Gradle build pulls only core plugins and the suite works
// offline beyond toolchain materialization.
//
// Listen-address resolution is bilingual, matching the Node fixture:
//   - `-addr HOST:PORT` argv wins when present (tests that set runtime{cmd});
//   - else PORT env on 127.0.0.1 (the inferred `java -jar` path passes no argv).
package echo;

import com.sun.net.httpserver.HttpExchange;
import com.sun.net.httpserver.HttpServer;
import java.io.OutputStream;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
import java.util.Map;

public final class Echo {
    public static void main(String[] args) throws Exception {
        String addr = flag(args, "-addr");
        String host;
        int port;
        if (addr != null) {
            int i = addr.lastIndexOf(':');
            host = addr.substring(0, i);
            port = Integer.parseInt(addr.substring(i + 1));
        } else {
            host = "127.0.0.1";
            String p = System.getenv("PORT");
            port = (p == null || p.isEmpty()) ? 8080 : Integer.parseInt(p);
        }

        byte[] payload = responseBody(args).getBytes(StandardCharsets.UTF_8);

        HttpServer server = HttpServer.create(new InetSocketAddress(host, port), 0);
        server.createContext("/", (HttpExchange ex) -> {
            ex.getResponseHeaders().set("Content-Type", "application/json");
            ex.sendResponseHeaders(200, payload.length);
            try (OutputStream os = ex.getResponseBody()) {
                os.write(payload);
            }
        });
        server.start();
        System.out.println("up " + host + ":" + port);
    }

    private static String flag(String[] args, String name) {
        for (int i = 0; i + 1 < args.length; i++) {
            if (args[i].equals(name)) {
                return args[i + 1];
            }
        }
        return null;
    }

    private static String responseBody(String[] args) {
        StringBuilder b = new StringBuilder();
        b.append('{');
        b.append("\"env\":{");
        boolean first = true;
        for (Map.Entry<String, String> e : System.getenv().entrySet()) {
            if (!first) {
                b.append(',');
            }
            first = false;
            b.append(quote(e.getKey())).append(':').append(quote(e.getValue()));
        }
        b.append("},");
        b.append("\"cwd\":").append(quote(System.getProperty("user.dir"))).append(',');
        b.append("\"argv\":[");
        for (int i = 0; i < args.length; i++) {
            if (i > 0) {
                b.append(',');
            }
            b.append(quote(args[i]));
        }
        b.append("],");
        b.append("\"runtime_version\":").append(quote(System.getProperty("java.version")));
        b.append('}');
        return b.toString();
    }

    private static String quote(String s) {
        StringBuilder b = new StringBuilder(s.length() + 2);
        b.append('"');
        for (int i = 0; i < s.length(); i++) {
            char c = s.charAt(i);
            switch (c) {
                case '"':
                    b.append("\\\"");
                    break;
                case '\\':
                    b.append("\\\\");
                    break;
                case '\n':
                    b.append("\\n");
                    break;
                case '\r':
                    b.append("\\r");
                    break;
                case '\t':
                    b.append("\\t");
                    break;
                default:
                    if (c < 0x20) {
                        b.append(String.format("\\u%04x", (int) c));
                    } else {
                        b.append(c);
                    }
            }
        }
        b.append('"');
        return b.toString();
    }

    private Echo() {
    }
}
