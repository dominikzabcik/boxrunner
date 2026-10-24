import SwiftUI

/// The app's mark: the fleet's glyph on a tinted tile.
struct Mark: View {
	var size: CGFloat = 30

	var body: some View {
		RoundedRectangle(cornerRadius: size * 0.27, style: .continuous)
			.fill(LinearGradient(colors: [Color(red: 0.36, green: 0.42, blue: 1), Color(red: 0.12, green: 0.72, blue: 0.86)], startPoint: .topLeading, endPoint: .bottomTrailing))
			.overlay(Image(systemName: "circle.hexagongrid.fill").font(.system(size: size * 0.52, weight: .semibold)).foregroundStyle(.white))
			.frame(width: size, height: size)
			.shadow(color: .black.opacity(0.15), radius: 1.5, y: 1)
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
		case .running: .blue
		case .idle: .green
		case .paused: .orange
		case .offline: .secondary
		}
	}
}

struct StatusDot: View {
	var health: Health

	var body: some View {
		Image(systemName: "circle.fill")
			.font(.system(size: 7))
			.foregroundStyle(health.color)
			.symbolEffect(.pulse, options: .repeating, isActive: health == .running)
			.frame(width: 12)
	}
}

/// One slot per runner the class may run: filled while a job runs, faint
/// while a runner starts, dashed past a limit set on the dashboard.
struct Slots: View {
	var `class`: Fleet.Class

	var body: some View {
		let allowed = min(self.class.limit?.max ?? self.class.max, self.class.max)
		HStack(spacing: 2.5) {
			ForEach(0 ..< max(self.class.max, 1), id: \.self) { i in
				let shape = RoundedRectangle(cornerRadius: 2, style: .continuous)
				if i < self.class.running {
					shape.fill(Color.blue)
				} else if i < self.class.running + self.class.starting {
					shape.fill(Color.blue.opacity(0.35))
				} else if i < allowed {
					shape.fill(Color.primary.opacity(0.1))
				} else {
					shape.strokeBorder(Color.primary.opacity(0.25), style: StrokeStyle(lineWidth: 1, dash: [2, 1.5]))
				}
			}
		}
		.frame(height: 9)
		.fixedSize()
	}
}

/// A figure with its label underneath, for the row of totals.
struct Figure: View {
	var value: Int
	var label: String
	var tint: Color = .primary

	var body: some View {
		VStack(spacing: 1) {
			Text("\(value)").font(.system(size: 22, weight: .semibold, design: .rounded)).monospacedDigit()
				.foregroundStyle(value == 0 ? Color.secondary : tint)
				.contentTransition(.numericText())
			Text(label).font(.caption2.weight(.medium)).foregroundStyle(.secondary).textCase(.uppercase).tracking(0.4)
		}
		.frame(maxWidth: .infinity)
	}
}

struct SectionTitle: View {
	var text: String
	var trailing: String?

	var body: some View {
		HStack {
			Text(text).font(.caption.weight(.semibold)).foregroundStyle(.secondary).textCase(.uppercase).tracking(0.5)
			Spacer()
			if let trailing { Text(trailing).font(.caption2).foregroundStyle(.tertiary) }
		}
	}
}

extension View {
	/// The quiet raised surface of one group in the menu.
	func panel(padding: CGFloat = 10) -> some View {
		self.padding(padding)
			.background(RoundedRectangle(cornerRadius: 10, style: .continuous).fill(Color.primary.opacity(0.05)))
			.overlay(RoundedRectangle(cornerRadius: 10, style: .continuous).strokeBorder(Color.primary.opacity(0.06)))
	}
}

/// A borderless icon button with a hover highlight, for the footer.
struct IconButton: View {
	var symbol: String
	var help: String
	var action: () -> Void
	@State private var hovering = false

	var body: some View {
		Button(action: action) {
			Image(systemName: symbol).font(.system(size: 13, weight: .medium))
				.frame(width: 26, height: 22)
				.background(RoundedRectangle(cornerRadius: 6, style: .continuous).fill(Color.primary.opacity(hovering ? 0.1 : 0)))
				.contentShape(Rectangle())
		}
		.buttonStyle(.plain)
		.foregroundStyle(.secondary)
		.help(help)
		.accessibilityLabel(help)
		.onHover { hovering = $0 }
	}
}
