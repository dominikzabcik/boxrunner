import SwiftUI

/// The menu's console look: one dark palette and one monospaced face, like a
/// terminal UI. It ignores the system's appearance on purpose.
enum Console {
	static let background = Color(hex: 0x0A0A0A)
	static let panel = Color(hex: 0x141414)
	static let line = Color(hex: 0x262626)
	static let text = Color(hex: 0xEEEEEE)
	static let muted = Color(hex: 0x808080)
	static let faint = Color(hex: 0x4A4A4A)
	static let accent = Color(hex: 0xFAB283)
	static let blue = Color(hex: 0x5C9CF5)
	static let green = Color(hex: 0x7FD88F)
	static let red = Color(hex: 0xE06C75)
	static let yellow = Color(hex: 0xE5C07B)

	static func font(_ size: CGFloat = 12, _ weight: Font.Weight = .regular) -> Font {
		.system(size: size, weight: weight, design: .monospaced)
	}
}

extension Color {
	init(hex: UInt32) {
		self.init(red: Double(hex >> 16 & 0xFF) / 255, green: Double(hex >> 8 & 0xFF) / 255, blue: Double(hex & 0xFF) / 255)
	}
}

/// A box's state as a colour: running, taking jobs, paused or offline.
enum Health {
	case running, idle, paused, offline

	init(_ box: Fleet.Box) {
		self = !box.online ? .offline : box.paused ? .paused : box.running > 0 ? .running : .idle
	}

	var color: Color {
		switch self {
		case .running: Console.blue
		case .idle: Console.green
		case .paused: Console.yellow
		case .offline: Console.faint
		}
	}
}

struct StatusDot: View {
	var health: Health

	var body: some View {
		Text("●").font(Console.font(9)).foregroundStyle(health.color)
			.phaseAnimator([1.0, 0.35], trigger: health == .running) { dot, opacity in
				dot.opacity(health == .running ? opacity : 1)
			} animation: { _ in .easeInOut(duration: 0.9) }
	}
}

/// One cell per runner the class may run: filled while a job runs, faint
/// while a runner starts, a dot past a limit set on the dashboard.
func slots(_ c: Fleet.Class) -> Text {
	let allowed = min(c.limit?.max ?? c.max, c.max)
	return (0 ..< max(c.max, 1)).reduce(Text("")) { text, i in
		if i < c.running { return text + Text("■").foregroundColor(Console.blue) }
		if i < c.running + c.starting { return text + Text("■").foregroundColor(Console.blue.opacity(0.4)) }
		if i < allowed { return text + Text("□").foregroundColor(Console.muted) }
		return text + Text("·").foregroundColor(Console.faint)
	}
}

/// A bordered block with its title set into the top line, as a TUI draws one,
/// and an accent bar down its left edge.
struct Block<Content: View>: View {
	var title: String
	var trailing: String?
	var accent: Color = Console.line
	@ViewBuilder var content: Content

	var body: some View {
		VStack(alignment: .leading, spacing: 6) {
			content
		}
		.frame(maxWidth: .infinity, alignment: .leading)
		.padding(.horizontal, 12).padding(.top, 13).padding(.bottom, 10)
		.background(Console.background)
		.overlay(alignment: .leading) { Rectangle().fill(accent).frame(width: 2) }
		.overlay(Rectangle().strokeBorder(Console.line, lineWidth: 1))
		.overlay(alignment: .top) {
			HStack(spacing: 0) {
				Text(" \(title) ").foregroundStyle(Console.muted).background(Console.background)
				Spacer()
				if let trailing { Text(" \(trailing) ").foregroundStyle(Console.faint).background(Console.background) }
			}
			.font(Console.font(10, .medium))
			.padding(.horizontal, 8)
			.offset(y: -7)
		}
	}
}

/// A label like `[ connect ⏎ ]`: brackets, a word and its key.
struct KeyButton: View {
	var title: String
	var key: String?
	var tint: Color = Console.text
	var filled = false
	var action: () -> Void
	@State private var hovering = false

	var body: some View {
		Button(action: action) {
			HStack(spacing: 6) {
				Text(title).foregroundStyle(filled ? Console.background : tint)
				if let key { Text(key).foregroundStyle(filled ? Console.background.opacity(0.6) : Console.muted) }
			}
			.font(Console.font(12, filled ? .semibold : .regular))
			.padding(.horizontal, 8).padding(.vertical, 4)
			.background(filled ? tint.opacity(hovering ? 0.85 : 1) : Console.line.opacity(hovering ? 1 : 0))
			.contentShape(Rectangle())
		}
		.buttonStyle(.plain)
		.onHover { hovering = $0 }
	}
}

/// ` ON | OFF `, a terminal's two-state switch for this Mac.
struct ConsoleSwitch: View {
	var on: Bool
	var disabled: Bool
	var toggle: () -> Void
	@State private var hovering = false

	var body: some View {
		Button(action: toggle) {
			HStack(spacing: 0) {
				Text(" ON ").foregroundStyle(on ? Console.background : Console.faint)
					.background(on ? Console.green : Color.clear)
				Text(" OFF ").foregroundStyle(on ? Console.faint : Console.background)
					.background(on ? Color.clear : Console.yellow)
			}
			.font(Console.font(11, .bold))
			.overlay(Rectangle().strokeBorder(hovering ? Console.muted : Console.line, lineWidth: 1))
			.contentShape(Rectangle())
		}
		.buttonStyle(.plain)
		.disabled(disabled)
		.opacity(disabled ? 0.5 : 1)
		.onHover { hovering = $0 }
		.accessibilityLabel(on ? "Available" : "Paused")
	}
}
