# [pinctrl module] (<https://github.com/mariapatni/pinctrl>)

The pinctrl module is a board module that accesses the hardware directly through /dev/mem.
to control GPIO pins. Pinctrl allows boards to switch between hardware PWM and GPIO modes.
Currently, the raspberry pi5 is supported.


### Models
`viam-labs:pinctrl:rpi5`

### Example Configuration
```json
  {
      "name": "pinctrl-pi5",
      "model": "viam-labs:pinctrl:rpi5",
      "type": "board",
      "namespace": "rdk",
      "attributes": {
      }
  }
  ```

Note that this module is experimential.








