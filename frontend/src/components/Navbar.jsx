import { useState } from 'react';
import { Link, useNavigate, useLocation } from 'react-router-dom';
import './Navbar.css';

function Navbar({ onRefresh, isRefreshing, title }) {
  const [isMenuOpen, setIsMenuOpen] = useState(false);
  const navigate = useNavigate();
  const location = useLocation();
  
  // Get page title from prop or determine from location
  const getPageTitle = () => {
    if (title) return title;
    if (location.pathname === '/') return 'Dashboard';
    if (location.pathname.startsWith('/group/')) {
      const parts = location.pathname.split('/');
      if (parts.length === 3) return decodeURIComponent(parts[2]);
      if (parts.length >= 5) return decodeURIComponent(parts[4]);
    }
    return 'Refity';
  };
  
  const pageTitle = getPageTitle();

  const handleLogout = async () => {
    if (window.confirm('Are you sure you want to logout?')) {
      const { authAPI } = await import('../services/api');
      await authAPI.logout();
      navigate('/login');
    }
  };

  const toggleMenu = () => {
    setIsMenuOpen(!isMenuOpen);
  };

  return (
    <nav className="navbar">
      <div className="navbar-container">
        <Link to="/" className="navbar-brand">
          <i className="bi bi-box-seam"></i>
          <span className="navbar-brand-text">Refity</span>
        </Link>
        
        {/* Mobile Title */}
        <span className="navbar-title-mobile">{pageTitle}</span>
        
        {/* Desktop Menu */}
        <div className="navbar-menu-desktop">
          <Link to="/profile" className="btn-nav">
            <i className="bi bi-person"></i>
            <span>Profile</span>
          </Link>
          {onRefresh && (
            <button
              onClick={onRefresh}
              className="btn-nav"
              disabled={isRefreshing}
            >
              {isRefreshing ? (
                <span className="spinner-border spinner-border-sm"></span>
              ) : (
                <i className="bi bi-arrow-clockwise"></i>
              )}
              <span>Refresh</span>
            </button>
          )}
          <button onClick={handleLogout} className="btn-nav">
            <i className="bi bi-box-arrow-right"></i>
            <span>Logout</span>
          </button>
        </div>

        {/* Mobile Hamburger Button */}
        <button 
          className="navbar-toggle"
          onClick={toggleMenu}
          aria-label="Toggle menu"
        >
          <i className={`bi ${isMenuOpen ? 'bi-x-lg' : 'bi-list'}`}></i>
        </button>
      </div>

      {/* Mobile Menu */}
      <div className={`navbar-menu-mobile ${isMenuOpen ? 'open' : ''}`}>
        <Link
          to="/profile"
          className="btn-nav-mobile"
          onClick={() => setIsMenuOpen(false)}
        >
          <i className="bi bi-person"></i>
          <span>Profile</span>
        </Link>
        {onRefresh && (
          <button
            onClick={() => {
              onRefresh();
              setIsMenuOpen(false);
            }}
            className="btn-nav-mobile"
            disabled={isRefreshing}
          >
            {isRefreshing ? (
              <span className="spinner-border spinner-border-sm"></span>
            ) : (
              <i className="bi bi-arrow-clockwise"></i>
            )}
            <span>Refresh</span>
          </button>
        )}
        <button onClick={handleLogout} className="btn-nav-mobile">
          <i className="bi bi-box-arrow-right"></i>
          <span>Logout</span>
        </button>
      </div>
    </nav>
  );
}

export default Navbar;
