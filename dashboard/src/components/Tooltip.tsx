import { useState, type ReactNode } from 'react';

interface TooltipProps {
  /** The popover body. Plain text or a small React tree. */
  content: ReactNode;
  /** Override the trigger glyph (default: a small "?" badge). */
  trigger?: ReactNode;
  /** Where the popover sits relative to the trigger. Default: top-right. */
  side?: 'top' | 'bottom' | 'left' | 'right';
}

const SIDE_CLASSES: Record<NonNullable<TooltipProps['side']>, string> = {
  // The popover is positioned just outside the trigger and nudged a few
  // pixels off so the connecting whitespace doesn't close the popover when
  // the user moves their cursor across it.
  top:    'bottom-full left-1/2 -translate-x-1/2 mb-2',
  bottom: 'top-full left-1/2 -translate-x-1/2 mt-2',
  left:   'right-full top-1/2 -translate-y-1/2 mr-2',
  right:  'left-full top-1/2 -translate-y-1/2 ml-2',
};

export function Tooltip({ content, trigger, side = 'top' }: TooltipProps) {
  // Tooltip is shown on hover OR keyboard focus so screen-reader and
  // keyboard-only users can still see the help text. The `?` button is
  // explicitly focusable.
  const [open, setOpen] = useState(false);
  return (
    <span className="relative inline-flex">
      <button
        type="button"
        aria-label="Help"
        onMouseEnter={() => setOpen(true)}
        onMouseLeave={() => setOpen(false)}
        onFocus={() => setOpen(true)}
        onBlur={() => setOpen(false)}
        onClick={(e) => {
          // Toggle on click too — mobile/touch has no hover state.
          e.preventDefault();
          setOpen((v) => !v);
        }}
        className="inline-flex items-center justify-center w-4 h-4 rounded-full border border-border text-[10px] text-muted hover:text-white hover:border-accent transition-colors"
      >
        {trigger ?? '?'}
      </button>
      {open && (
        <span
          role="tooltip"
          className={`absolute z-50 w-64 px-3 py-2 bg-panel border border-border rounded-md shadow-lg text-[11px] text-text leading-relaxed pointer-events-auto ${SIDE_CLASSES[side]}`}
          // The popover should be selectable / link-clickable, hence
          // pointer-events-auto. We don't bind onMouseLeave on the popover
          // because the parent wrapper's hover state already covers it on
          // adjacent moves; if the cursor jumps away the button's onMouseLeave
          // fires and closes it.
        >
          {content}
        </span>
      )}
    </span>
  );
}
