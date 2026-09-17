import { forwardRef } from 'react'
import styles from './Button.module.css'

interface ButtonProps extends React.ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: 'primary' | 'secondary' | 'ghost' | 'danger'
  size?: 'sm' | 'md' | 'lg'
}

export const Button = forwardRef<HTMLButtonElement, ButtonProps>(
  ({ variant = 'primary', size = 'md', className, children, ...props }, ref) => {
    const cls = [styles.btn, styles[variant], styles[size], className]
      .filter(Boolean)
      .join(' ')

    return (
      <button ref={ref} className={cls} {...props}>
        {children}
      </button>
    )
  },
)

Button.displayName = 'Button'
