import './Footer.css';

function Footer() {
  return (
    <footer className="app-footer">
      <div className="footer-content">
        <div className="footer-left">
          <span className="footer-text">
            © 2025 – {new Date().getFullYear()}. All rights reserved.
          </span>
        </div>
      </div>
    </footer>
  );
}

export default Footer;
