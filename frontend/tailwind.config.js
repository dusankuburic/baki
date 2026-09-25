/** @type {import('tailwindcss').Config} */

// Every theme colour is a CSS variable. Tailwind cannot add alpha to a bare
// `var(--x)`, so `bg-brand-500/10`-style tints generated NOTHING (282 usages
// were silently dropped) and a bare `ring-1` fell back to Tailwind's blue.
// Wrapping each token in color-mix with the <alpha-value> placeholder fixes
// both: `/10` becomes `calc(0.1 * 100%)`, and the plain utility keeps the
// `--tw-*-opacity` variable at 1, which renders exactly as before.
const token = name => `color-mix(in srgb, var(--${name}) calc(<alpha-value> * 100%), transparent)`

// The ring plugin resolves its default colour at build time, outside the
// utility pipeline that understands <alpha-value>, so this one is a function.
const ringDefault = ({opacityValue = 1}) =>
  `color-mix(in srgb, var(--brand-500) calc(${opacityValue} * 100%), transparent)`

export default {
  // Test files are excluded: classNames in fixtures/mocks would otherwise be
  // extracted as production utilities (pure CSS bloat in the built sheet).
  content: ['./index.html', './src/**/*.{ts,tsx}', '!./src/**/*.test.*'],
  darkMode: [
    'class',
    '[data-theme="dark"], [data-theme="midnight"], [data-theme="warm"], [data-theme="tokyo-night"], [data-theme="one-dark"], [data-theme="dracula"], [data-theme="nord"], [data-theme="gruvbox-dark"], [data-theme="catppuccin-mocha"], [data-theme="rose-pine"], [data-theme="rose-pine-moon"], [data-theme="github-dark"], [data-theme="kanagawa"], [data-theme="everforest"]',
  ],
  theme: {
    extend: {
      colors: {
        surface: {
          0: token('surface-0'),
          1: token('surface-1'),
          2: token('surface-2'),
          3: token('surface-3'),
          4: token('surface-4'),
          // Already translucent (rgba): the Modal / palette backdrop.
          overlay: 'var(--surface-overlay)',
        },
        text: {
          primary: token('text-primary'),
          secondary: token('text-secondary'),
          tertiary: token('text-tertiary'),
          disabled: token('text-disabled'),
        },
        border: {
          subtle: token('border-subtle'),
          DEFAULT: token('border-default'),
          // `border-border-default` is written all over the app; without this
          // alias it generated nothing (the plain `border` default carried it).
          default: token('border-default'),
          strong: token('border-strong'),
          focus: token('border-focus'),
        },
        brand: {
          50: token('brand-50'),
          100: token('brand-100'),
          300: token('brand-300'),
          400: token('brand-400'),
          500: token('brand-500'),
          600: token('brand-600'),
          700: token('brand-700'),
          foreground: token('brand-foreground'),
        },
        block: {
          action: token('block-action'),
          loop: token('block-loop'),
          condition: token('block-condition'),
          subflow: token('block-subflow'),
          error: token('block-error'),
          comment: token('block-comment'),
          variable: token('block-variable'),
          string: token('block-string'),
          wait: token('block-wait'),
          'action-bg': token('block-action-bg'),
          'loop-bg': token('block-loop-bg'),
          'condition-bg': token('block-condition-bg'),
          'subflow-bg': token('block-subflow-bg'),
          'error-bg': token('block-error-bg'),
          'comment-bg': token('block-comment-bg'),
          'variable-bg': token('block-variable-bg'),
          'string-bg': token('block-string-bg'),
          'wait-bg': token('block-wait-bg'),
        },
        semantic: {
          success: token('success'),
          warning: token('warning'),
          error: token('error'),
          info: token('info'),
        },
        // Data-viz series slots and chart furniture (index.css "DATA VIZ").
        viz: {
          1: token('viz-1'),
          2: token('viz-2'),
          3: token('viz-3'),
          4: token('viz-4'),
          5: token('viz-5'),
          6: token('viz-6'),
          7: token('viz-7'),
          8: token('viz-8'),
          emphasis: token('viz-emphasis'),
          muted: token('viz-muted'),
        },
      },
      borderColor: {
        DEFAULT: 'var(--border-default)',
      },
      ringColor: {
        DEFAULT: ringDefault,
      },
      fontFamily: {
        sans: 'var(--font-sans)',
        mono: 'var(--font-mono)',
      },
      fontSize: {
        '2xs': ['10px', {lineHeight: '14px'}],
        xs: ['11px', {lineHeight: '16px'}],
        sm: ['13px', {lineHeight: '18px'}],
        base: ['15px', {lineHeight: '22px'}],
        lg: ['17px', {lineHeight: '24px'}],
        xl: ['20px', {lineHeight: '28px'}],
        '2xl': ['28px', {lineHeight: '36px', letterSpacing: '-0.01em'}],
        '3xl': ['36px', {lineHeight: '44px', letterSpacing: '-0.02em'}],
      },
      // A custom-property name cannot contain a dot: `--space-1.5` was dropped
      // by browsers and every half-step utility (p-1.5, gap-1.5, py-0.5, h-3.5)
      // computed to 0. The half steps use an underscore in index.css.
      spacing: {
        0.5: 'var(--space-0_5)',
        1: 'var(--space-1)',
        1.5: 'var(--space-1_5)',
        2: 'var(--space-2)',
        2.5: 'var(--space-2_5)',
        3: 'var(--space-3)',
        3.5: 'var(--space-3_5)',
        4: 'var(--space-4)',
        5: 'var(--space-5)',
        6: 'var(--space-6)',
        8: 'var(--space-8)',
        10: 'var(--space-10)',
        12: 'var(--space-12)',
        16: 'var(--space-16)',
      },
      borderRadius: {
        xs: 'var(--radius-xs)',
        sm: 'var(--radius-sm)',
        md: 'var(--radius-md)',
        lg: 'var(--radius-lg)',
        xl: 'var(--radius-xl)',
        '2xl': 'var(--radius-2xl)',
      },
      zIndex: {
        base: 'var(--z-base)',
        raised: 'var(--z-raised)',
        overlay: 'var(--z-overlay)',
        sticky: 'var(--z-sticky)',
        modal: 'var(--z-modal)',
        tooltip: 'var(--z-tooltip)',
        toast: 'var(--z-toast)',
      },
      boxShadow: {
        xs: 'var(--shadow-xs)',
        sm: 'var(--shadow-sm)',
        md: 'var(--shadow-md)',
        lg: 'var(--shadow-lg)',
        xl: 'var(--shadow-xl)',
        glow: 'var(--shadow-glow)',
        'glow-error': 'var(--shadow-glow-error)',
      },
      transitionTimingFunction: {
        out: 'var(--ease-out)',
        'in-out': 'var(--ease-in-out)',
        spring: 'var(--ease-spring)',
      },
      transitionDuration: {
        instant: 'var(--duration-instant)',
        fast: 'var(--duration-fast)',
        normal: 'var(--duration-normal)',
        slow: 'var(--duration-slow)',
        slower: 'var(--duration-slower)',
      },
      backdropBlur: {
        glass: '16px',
      },
    },
  },
  plugins: [],
}
