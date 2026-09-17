import { forwardRef, useId } from 'react'
import styles from './Input.module.css'

interface InputProps extends React.InputHTMLAttributes<HTMLInputElement> {
  label?: string
  error?: string
}

export const Input = forwardRef<HTMLInputElement, InputProps>(
  ({ label, error, className, id: idProp, ...props }, ref) => {
    const generated = useId()
    const id = idProp ?? generated

    return (
      <div className={styles.field}>
        {label && <label className={styles.label} htmlFor={id}>{label}</label>}
        <input
          ref={ref}
          id={id}
          className={`${styles.input} ${error ? styles.error : ''} ${className ?? ''}`}
          {...props}
        />
        {error && <span className={styles.errorText}>{error}</span>}
      </div>
    )
  },
)

Input.displayName = 'Input'
