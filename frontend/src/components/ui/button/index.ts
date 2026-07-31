import type { VariantProps } from "class-variance-authority"
import { cva } from "class-variance-authority"

export { default as Button } from "./Button.vue"

export const buttonVariants = cva(
  "btn-press inline-flex items-center justify-center gap-2 whitespace-nowrap rounded-lg text-sm font-medium transition-all duration-200 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 focus-visible:ring-offset-background disabled:pointer-events-none disabled:opacity-50 [&_svg]:pointer-events-none [&_svg]:size-4 [&_svg]:shrink-0",
  {
    variants: {
      variant: {
        // ReReply olive primary button with a restrained brand glow
        default: "bg-gradient-to-r from-[#5b613c] to-[#747c4d] text-white shadow-lg shadow-[#4e5233]/25 hover:shadow-[#697046]/35 hover:from-[#697046] hover:to-[#838c57]",
        destructive:
          "bg-gradient-to-r from-red-500 to-rose-600 text-white shadow-lg shadow-red-500/25 hover:shadow-red-500/40",
        // Glass outline for dark mode
        outline:
          "border border-white/10 bg-white/[0.02] text-foreground hover:bg-white/[0.06] hover:border-white/20 light:border-slate-300 light:bg-slate-50 light:hover:bg-slate-200",
        // Active/selected state - works in both dark and light modes
        active:
          "border border-primary/50 bg-primary text-primary-foreground light:bg-primary light:text-primary-foreground light:border-primary/50",
        secondary:
          "bg-white/[0.06] text-foreground hover:bg-white/[0.10] light:bg-slate-200 light:hover:bg-slate-300",
        // Subtle ghost with hover
        ghost: "text-muted-foreground hover:bg-white/[0.06] hover:text-foreground light:hover:bg-slate-200",
        link: "text-primary underline-offset-4 hover:underline",
        // Glass variant for cards/panels
        glass: "bg-white/[0.04] border border-white/[0.08] text-foreground hover:bg-white/[0.08] light:bg-slate-100 light:border-slate-300 light:hover:bg-slate-200",
      },
      size: {
        "default": "h-9 px-4 py-2",
        "xs": "h-7 rounded-md px-2",
        "sm": "h-8 rounded-lg px-3 text-xs",
        "lg": "h-11 rounded-lg px-8",
        "icon": "h-9 w-9",
        "icon-sm": "size-8",
        "icon-lg": "size-10",
      },
    },
    defaultVariants: {
      variant: "default",
      size: "default",
    },
  },
)

export type ButtonVariants = VariantProps<typeof buttonVariants>
