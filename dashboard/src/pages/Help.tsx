import { Link } from 'react-router-dom';
import { useTelemetryStream } from '../context/WebSocketContext';

function Section({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <div className="bg-panel border border-border rounded-lg p-5 mb-4">
      <h2 className="text-base font-bold text-white mb-3">{title}</h2>
      <div className="text-sm text-text leading-relaxed space-y-3">{children}</div>
    </div>
  );
}

function CodeBlock({ children }: { children: React.ReactNode }) {
  return (
    <pre className="bg-bg border border-border rounded-md px-3 py-2 text-xs text-accent font-mono whitespace-pre-wrap">
      {children}
    </pre>
  );
}

export default function Help() {
  const { health } = useTelemetryStream();
  return (
    <div className="h-full overflow-y-auto p-6">
      <div className="mb-6">
        <h1 className="text-2xl font-bold text-white">Help</h1>
        <p className="text-sm text-muted mt-1">
          Getting started, voice commands, and troubleshooting for the Race Engineer
          coach.
        </p>
      </div>

      <Section title="Getting started">
        <p>
          Race Engineer needs three things running: the F1 25 game streaming UDP
          telemetry, the Go telemetry core (which is what this dashboard talks
          to), and the Python voice service if you want voice replies.
        </p>
        <ol className="list-decimal pl-5 space-y-1 text-sm">
          <li>
            In F1 25 → <em>Settings → Telemetry Settings</em>, enable UDP, set the
            IP to broadcast (255.255.255.255) or this machine's IP, and use port{' '}
            <code className="text-accent">20777</code>.
          </li>
          <li>
            From the project root, run <code className="text-accent">make start</code>{' '}
            — this builds the Go binary, opens the dashboard at{' '}
            <code className="text-accent">http://localhost:5173</code>, and launches
            the voice service on port 8000.
          </li>
          <li>
            Drive an out-lap. The connection dot in the sidebar should flip green
            once UDP packets arrive.
          </li>
        </ol>
      </Section>

      <Section title="Voice commands & PTT">
        <p>
          Push-to-talk maps a wheel button to the radio mic. The button bitmask
          lives in <Link to="/settings" className="text-accent hover:underline">Settings → Push-to-Talk</Link>.
          Default is the PS5 Cross / Xbox A button (
          <code className="text-accent">0x00000001</code>).
        </p>
        <p>
          Hold the mic and ask things like:
        </p>
        <ul className="list-disc pl-5 space-y-1 text-sm">
          <li>"How are the rear tyres looking?"</li>
          <li>"What's the gap to Hamilton?"</li>
          <li>"When should I box?"</li>
          <li>"Is rain coming?"</li>
        </ul>
        <p className="text-muted text-xs">
          Tip: enable <code>LOG_BUTTONS</code> from Settings → PTT to print every
          BUTN press to the server log while you map a wheel button to its bitmask.
        </p>
      </Section>

      <Section title="UDP setup walkthrough">
        <p>
          Two stream modes are supported and both live in Settings → Telemetry:
        </p>
        <ul className="list-disc pl-5 space-y-1 text-sm">
          <li>
            <strong className="text-white">Broadcast</strong> — game broadcasts to
            every device on your LAN. Easiest mode; works without knowing your IP.
          </li>
          <li>
            <strong className="text-white">Unicast</strong> — game targets one
            specific IP. Use this if your LAN blocks broadcast traffic.
          </li>
        </ul>
        <CodeBlock>{`Current UDP host: ${health?.udp_host ?? '0.0.0.0'}
Current UDP port: ${health?.udp_port ?? 20777}
Mode:             ${(health?.udp_mode ?? 'broadcast').toUpperCase()}`}</CodeBlock>
      </Section>

      <Section title="Troubleshooting">
        <ul className="list-disc pl-5 space-y-2 text-sm">
          <li>
            <strong className="text-white">No telemetry showing up:</strong> verify
            the F1 25 telemetry UDP is enabled and pointed at this machine, then
            check the sidebar dot (red = no packets received). Restart{' '}
            <code className="text-accent">make start</code> after changing the UDP
            port from Settings.
          </li>
          <li>
            <strong className="text-white">Engineer says nothing:</strong> the
            voice service runs on port 8000 — confirm the process is running and
            an LLM API key is set in{' '}
            <Link to="/settings" className="text-accent hover:underline">Settings → LLM</Link>.
          </li>
          <li>
            <strong className="text-white">Microphone doesn't trigger:</strong>{' '}
            check the BUTN bitmask matches your wheel; turn on{' '}
            <code className="text-accent">LOG_BUTTONS</code> and watch the server
            log when you press the wheel button.
          </li>
          <li>
            <strong className="text-white">Career stats look wrong:</strong> the
            Home page derives the player car from telemetry_hifreq. If you've been
            running with{' '}
            <code className="text-accent">HIFREQ_SAMPLE_RATE=0</code>, historical
            sessions fall back to a "most laps recorded" heuristic that can pick
            the wrong car for races.
          </li>
        </ul>
      </Section>

      <Section title="About">
        <p>
          Race Engineer is an open-source pit-wall and coaching platform for F1 25.
          Telemetry → DuckDB → live dashboard + Gemini Live voice agent.
        </p>
        <p className="text-muted text-xs">
          Active LLM provider:{' '}
          <code className="text-accent">unknown</code> · Persisted config in{' '}
          <code className="text-accent">~/.race-engineer/config.json</code>.
        </p>
        <div className="flex gap-3 text-xs">
          <a
            href="https://github.com/iamtushar324/race-engineer"
            target="_blank"
            rel="noreferrer"
            className="text-accent hover:underline"
          >
            GitHub
          </a>
          <span className="text-border">·</span>
          <a
            href="/CLAUDE.md"
            target="_blank"
            rel="noreferrer"
            className="text-accent hover:underline"
          >
            CLAUDE.md
          </a>
        </div>
      </Section>
    </div>
  );
}
